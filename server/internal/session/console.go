// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/valkey-io/valkey-go"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/audit"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/mfa"
	"github.com/akari-project/panel/server/internal/ratelimit"
)

// 管理会话（AUTH-21）：刷新令牌空闲 30 分钟失效，自登录起 12 小时绝对失效。
const (
	ConsoleIdle     = 30 * time.Minute
	ConsoleAbsolute = 12 * time.Hour
	// StepUpTTL 是 Mfa-Assertion 的有效期（AUTH-19）。
	StepUpTTL = 5 * time.Minute
)

// ConsoleAMR 是管理令牌的 amr：管理会话只能由完成二次验证的登录建立（AUTH-21）。
var ConsoleAMR = []string{"pwd", "otp"}

// StepUpPerSession 限制每个管理会话的 step-up 尝试次数（包括成功），失败另计入 AUTH-09 的账号失败次数。
var StepUpPerSession = ratelimit.Rule{Name: "step-up-session", Limit: 5, Window: 15 * time.Minute}

// StepUpMethods 是 step-up 可用的方式（AUTH-19）：只有 TOTP（Passkey 在 M4），恢复码不可用。
var StepUpMethods = []string{"totp"}

// consoleChallenge 是管理员登录的二次验证挑战（AUTH-20、AUTH-21）。与客户端的挑战分开存放，互不通用。
// 尚未绑定 TOTP 的管理员，挑战中带待确认的密钥（以账号与挑战 ID 为附加数据加密，CONV-19）。
type consoleChallenge struct {
	Account uuid.UUID `json:"a"`
	Email   string    `json:"e"`
	Enroll  []byte    `json:"s,omitempty"`
}

func consoleChallengeKey(id uuid.UUID) string { return "mfa:console-challenge:" + id.String() }
func consoleAttemptsKey(id uuid.UUID) string  { return "mfa:console-challenge:" + id.String() + ":n" }
func enrollAD(account, challenge uuid.UUID) []byte {
	return []byte("console.enrollment:" + account.String() + ":" + challenge.String())
}

// stepUpKey 以 Mfa-Assertion 的 SHA-256 为键：Valkey 中不保存明文（CONV-20 的做法）。
func stepUpKey(assertion string) string {
	sum := sha256.Sum256([]byte(assertion))
	return "console:step-up:" + hex.EncodeToString(sum[:])
}

// ConsoleLogin 是管理员登录请求。
type ConsoleLogin struct {
	Email, Password string
	IP              string // 限流主体
	IPPrefix        string // /24 或 /48（CONV-24）
	UserAgent       string
}

// ConsoleMFALogin 是管理员登录第二步。
type ConsoleMFALogin struct {
	ChallengeID            uuid.UUID
	TOTPCode, RecoveryCode string
	IP, IPPrefix           string
	UserAgent              string
}

// ConsoleResult 是管理员登录的结果。
type ConsoleResult struct {
	Tokens
	AccountID uuid.UUID
	// RecoveryCodes 为首次登录同时完成 TOTP 绑定时生成的恢复码，只返回一次。
	RecoveryCodes []string
}

func (s *Service) warn(ctx context.Context, msg string, account uuid.UUID) {
	if s.Log == nil {
		return
	}
	attrs := []slog.Attr{}
	if account != uuid.Nil {
		attrs = append(attrs, slog.String("account_id", account.String()))
	}
	s.Log.LogAttrs(ctx, slog.LevelWarn, msg, attrs...)
}

// ConsolePasswordLogin 是管理员登录第一步（AUTH-09、AUTH-20、AUTH-21）：
//   - 邮箱不存在与密码错误返回相同的 401 unauthenticated，并都恰好执行一次 argon2id；
//   - 密码正确时：账号不在正常状态返回 403 account_suspended，不是管理员返回 403 forbidden，
//     否则一律返回 401 mfa_required 并附 challenge_id 与 methods；
//   - 尚未绑定 TOTP 的管理员另附 totp_enrollment，第二步提交 TOTP 码即完成绑定（AUTH-21）。
//
// 失败不写审计（只追加表不能清理，且没有可信的操作者），记 warn 日志，只记已知的账号 ID（AUTH-18、CONV-24）。
func (s *Service) ConsolePasswordLogin(ctx context.Context, in ConsoleLogin) error {
	email, err := normalize(in.Email)
	if err != nil {
		return err
	}
	if in.Password == "" || len(in.Password) > 4*128 {
		return apierr.Invalid(apierr.Field("password", "invalid_format"))
	}
	if err := s.reserveAttempt(ctx, in.IP, email); err != nil {
		return err
	}
	q := sqlc.New(s.Pool)
	acct, err := q.LoginAccount(ctx, email)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	hash := DummyHash
	if found && acct.PasswordHash != nil {
		hash = *acct.PasswordHash
	}
	ok, err := s.verify(in.Password, hash)
	if err != nil {
		return err
	}
	if !found || acct.PasswordHash == nil || !ok {
		if found {
			s.warn(ctx, "console login failed", acct.ID)
		}
		return apierr.Unauthenticated
	}
	if acct.Status != "active" {
		return apierr.New(403, "account_suspended")
	}
	roles, err := q.AccountRoles(ctx, acct.ID)
	if err != nil {
		return err
	}
	if len(roles) == 0 {
		return apierr.Forbidden
	}
	c := consoleChallenge{Account: acct.ID, Email: email}
	id := uuid.New()
	enabled, _, err := mfa.Enabled(ctx, q, acct.ID)
	if err != nil {
		return err
	}
	methods := []string{"totp", "recovery_code"}
	var enrollment map[string]string
	if !enabled {
		secret, encoded, uri, err := s.MFA.NewSecret(email)
		if err != nil {
			return err
		}
		if c.Enroll, err = s.Keys.Seal(secret, enrollAD(acct.ID, id)); err != nil {
			return err
		}
		methods = []string{"totp"}
		enrollment = map[string]string{"secret": encoded, "otpauth_uri": uri}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := s.kv(ctx, s.KV.B().Set().Key(consoleChallengeKey(id)).Value(string(b)).Ex(ChallengeTTL).Build())[0].Error(); err != nil {
		return apierr.Unavailable(err)
	}
	e := apierr.New(401, "mfa_required").With("challenge_id", id).With("methods", methods)
	if enrollment != nil {
		e = e.With("totp_enrollment", enrollment)
	}
	return e
}

// ConsoleCompleteLogin 是管理员登录第二步（AUTH-20、AUTH-21）：challenge_id 与 TOTP 码或恢复码之一。
// 首次绑定时只接受 TOTP 码，成功后启用 TOTP 并返回恢复码。每次尝试预占 AUTH-09 的账号失败名额，成功后清除；
// 每个挑战最多尝试 5 次。挑战不存在、已过期或码不正确一律返回 401 unauthenticated。
// 成功时签发受众为 console 的会话（不注册设备），并写审计 session.create。
func (s *Service) ConsoleCompleteLogin(ctx context.Context, in ConsoleMFALogin) (ConsoleResult, error) {
	if (in.TOTPCode == "") == (in.RecoveryCode == "") {
		return ConsoleResult{}, apierr.Invalid(apierr.Field("totp_code", "required"))
	}
	raw, err := s.kv(ctx, s.KV.B().Get().Key(consoleChallengeKey(in.ChallengeID)).Build())[0].AsBytes()
	if valkey.IsValkeyNil(err) {
		return ConsoleResult{}, apierr.Unauthenticated
	}
	if err != nil {
		return ConsoleResult{}, apierr.Unavailable(err)
	}
	var c consoleChallenge
	if err := json.Unmarshal(raw, &c); err != nil {
		return ConsoleResult{}, apierr.Unauthenticated
	}
	if c.Enroll != nil && in.RecoveryCode != "" {
		return ConsoleResult{}, apierr.Invalid(apierr.Field("recovery_code", "not_allowed"))
	}
	if err := s.reserveAttempt(ctx, in.IP, c.Email); err != nil {
		return ConsoleResult{}, err
	}
	n, err := s.kv(ctx,
		s.KV.B().Incr().Key(consoleAttemptsKey(in.ChallengeID)).Build(),
		s.KV.B().Expire().Key(consoleAttemptsKey(in.ChallengeID)).Seconds(int64(ChallengeTTL/time.Second)).Build(),
	)[0].AsInt64()
	if err != nil {
		return ConsoleResult{}, apierr.Unavailable(err)
	}
	if n > ChallengeMaxAttempts {
		s.dropConsoleChallenge(ctx, in.ChallengeID)
		return ConsoleResult{}, apierr.Unauthenticated
	}
	var secret []byte
	if c.Enroll != nil {
		if secret, err = s.Keys.Open(c.Enroll, enrollAD(c.Account, in.ChallengeID)); err != nil {
			return ConsoleResult{}, apierr.Unauthenticated
		}
	}

	var (
		res     = ConsoleResult{AccountID: c.Account}
		denied  error
		ok      bool
		revoked []uuid.UUID
	)
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.LockAccount(ctx, c.Account); err != nil {
			return err
		}
		a, err := q.AccountByID(ctx, c.Account)
		if err != nil {
			return err
		}
		// 先检查账号状态与角色：被暂停或已不是管理员的账号不消耗恢复码、不启用 TOTP。
		if a.Status != "active" {
			denied = apierr.New(403, "account_suspended")
			return nil
		}
		roles, err := q.AccountRoles(ctx, c.Account)
		if err != nil {
			return err
		}
		if len(roles) == 0 {
			denied = apierr.Forbidden
			return nil
		}
		if secret != nil {
			res.RecoveryCodes, revoked, err = s.MFA.Enable(ctx, q, c.Account, secret, in.TOTPCode)
			if errors.Is(err, mfa.ErrIncorrect) {
				return nil
			}
			if err != nil {
				return err
			}
		} else if ok, err = s.MFA.Verify(ctx, q, c.Account, in.TOTPCode, in.RecoveryCode); err != nil || !ok {
			return err
		}
		ok = true
		now := s.Clock.Now()
		if res.Tokens, err = s.newSession(ctx, q, c.Account, uuid.Nil, token.AudienceConsole, nil, in.UserAgent, in.IPPrefix,
			ConsoleIdle, now.Add(ConsoleAbsolute), ConsoleAMR); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Action: "session.create", TargetType: "account", TargetID: c.Account.String(), Actor: &c.Account,
		})
	})
	if err != nil {
		return ConsoleResult{}, err
	}
	if denied != nil {
		s.dropConsoleChallenge(ctx, in.ChallengeID)
		return ConsoleResult{}, denied
	}
	if !ok {
		s.warn(ctx, "console login second step failed", c.Account)
		if n >= ChallengeMaxAttempts {
			s.dropConsoleChallenge(ctx, in.ChallengeID)
		}
		return ConsoleResult{}, apierr.Unauthenticated
	}
	s.dropConsoleChallenge(ctx, in.ChallengeID)
	if err := s.revokeAfterCommit(ctx, revoked); err != nil {
		return ConsoleResult{}, apierr.Unavailable(err)
	}
	if err := s.clearAttempts(ctx, c.Email); err != nil {
		return ConsoleResult{}, err
	}
	return res, nil
}

func (s *Service) dropConsoleChallenge(ctx context.Context, id uuid.UUID) {
	s.kv(ctx, s.KV.B().Del().Key(consoleChallengeKey(id), consoleAttemptsKey(id)).Build())
}

// ConsoleLogout 吊销当前管理会话所在的会话链，并写审计 session.delete（AUTH-18）。
// 会话链被吊销后，签发给它的 Mfa-Assertion 随之失效（AUTH-19）。
func (s *Service) ConsoleLogout(ctx context.Context, p auth.Principal) error {
	var revoked []uuid.UUID
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		var err error
		if revoked, err = revokeChain(ctx, q, p.SessionID, s.Clock.Now()); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: "session.delete", TargetType: "account", TargetID: p.AccountID.String()})
	})
	if err != nil {
		return err
	}
	revoked = append(revoked, p.SessionID)
	if err := s.revokeAfterCommit(ctx, revoked); err != nil {
		return apierr.Unavailable(err)
	}
	return nil
}

// stepUp 是 Valkey 中保存的 Mfa-Assertion：绑定账号与签发时的会话链（以根会话标识，AUTH-19）。
// 失效时间按注入的时钟判断（CONV-04）；Valkey 的 TTL 只作为清理的上界。
type stepUp struct {
	Account uuid.UUID `json:"a"`
	Chain   uuid.UUID `json:"c"`
	Expires int64     `json:"e"` // Unix 毫秒
}

// StepUp 以 TOTP 码完成一次重新验证，签发 5 分钟有效的 Mfa-Assertion（AUTH-19）：
//   - 只接受 TOTP（恢复码不可用于 step-up）；码不正确返回 400 incorrect，计入 AUTH-09 的账号失败次数；
//   - Mfa-Assertion 为 32 字节随机值（不透明，与访问令牌不同，不能互相替代），Valkey 中只保存其 SHA-256，
//     绑定账号与会话链；有效期内可以用于多个敏感操作；
//   - 成功写审计 step_up.create；失败不写审计，只记 warn 日志。
func (s *Service) StepUp(ctx context.Context, p auth.Principal, totpCode string) (string, time.Time, error) {
	if totpCode == "" {
		return "", time.Time{}, apierr.Invalid(apierr.Field("totp_code", "required"))
	}
	q := sqlc.New(s.Pool)
	a, err := q.AccountByID(ctx, p.AccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, apierr.Unauthenticated
	}
	if err != nil {
		return "", time.Time{}, err
	}
	allowed, retry, err := s.Limiter.Allow(ctx, StepUpPerSession, p.SessionID.String())
	if err != nil {
		return "", time.Time{}, apierr.Unavailable(err)
	}
	if !allowed {
		return "", time.Time{}, apierr.RateLimited(retry)
	}
	chain, err := q.SessionChainRoot(ctx, p.SessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, apierr.Unauthenticated
	}
	if err != nil {
		return "", time.Time{}, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	assertion := base64.RawURLEncoding.EncodeToString(b)
	expires := s.Clock.Now().Add(StepUpTTL)
	rec, err := json.Marshal(stepUp{Account: p.AccountID, Chain: chain, Expires: expires.UnixMilli()})
	if err != nil {
		return "", time.Time{}, err
	}
	var ok bool
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if ok, err = s.MFA.Verify(ctx, q, p.AccountID, totpCode, ""); err != nil || !ok {
			return err
		}
		if err := audit.Record(ctx, q, audit.Entry{Action: "step_up.create", TargetType: "account", TargetID: p.AccountID.String()}); err != nil {
			return err
		}
		// 在提交之前写入 Valkey：写入失败则回滚，不留下没有对应令牌的审计记录与已消耗的时间步。
		if err := s.kv(ctx, s.KV.B().Set().Key(stepUpKey(assertion)).Value(string(rec)).Ex(StepUpTTL).Build())[0].Error(); err != nil {
			return apierr.Unavailable(err)
		}
		return nil
	})
	if err != nil {
		return "", time.Time{}, err
	}
	if !ok {
		s.warn(ctx, "console step-up failed", p.AccountID)
		email, _ := normalize(a.Email)
		if err := s.recordFailure(ctx, email); err != nil {
			return "", time.Time{}, err
		}
		return "", time.Time{}, apierr.Invalid(apierr.Field("totp_code", "incorrect"))
	}
	return assertion, expires, nil
}

// ErrStepUpRequired 是缺少、过期或不属于当前会话链的 Mfa-Assertion（AUTH-19）：
// methods 只列出 step-up 可用的方式，不附 challenge_id。
var ErrStepUpRequired = apierr.New(401, "mfa_required").With("methods", StepUpMethods)

// CheckStepUp 校验敏感操作的 Mfa-Assertion：必须由同一账号、同一会话链在 5 分钟内签发（AUTH-19）。
// 会话链被吊销后，访问令牌已被拒绝，Mfa-Assertion 随之不可用。
func (s *Service) CheckStepUp(ctx context.Context, p auth.Principal, assertion string) error {
	if assertion == "" || len(assertion) > 128 {
		return ErrStepUpRequired
	}
	raw, err := s.kv(ctx, s.KV.B().Get().Key(stepUpKey(assertion)).Build())[0].AsBytes()
	if valkey.IsValkeyNil(err) {
		return ErrStepUpRequired
	}
	if err != nil {
		return apierr.Unavailable(err)
	}
	var rec stepUp
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Account != p.AccountID || !s.Clock.Now().Before(time.UnixMilli(rec.Expires)) {
		return ErrStepUpRequired
	}
	chain, err := sqlc.New(s.Pool).SessionChainRoot(ctx, p.SessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStepUpRequired
	}
	if err != nil {
		return err
	}
	if chain != rec.Chain {
		return ErrStepUpRequired
	}
	return nil
}
