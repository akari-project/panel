// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/valkey-io/valkey-go"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/mfa"
	"github.com/akari-project/panel/server/internal/notify"
	"github.com/akari-project/panel/server/internal/password"
	"github.com/akari-project/panel/server/internal/ratelimit"
)

// 二次验证挑战（AUTH-20）：5 分钟有效，最多尝试 5 次。重新验证（AUTH-23）5 分钟内有效。
const (
	ChallengeTTL         = 5 * time.Minute
	ChallengeMaxAttempts = 5
	ReauthTTL            = 5 * time.Minute
)

// ReauthPerSession 限制每个会话的重新验证尝试次数（包括成功）。
var ReauthPerSession = ratelimit.Rule{Name: "reauth-session", Limit: 5, Window: 15 * time.Minute}

type challenge struct {
	Account   uuid.UUID `json:"a"`
	Email     string    `json:"e"`
	Platform  string    `json:"p"`
	PublicKey string    `json:"k,omitempty"`
}

func challengeKey(id uuid.UUID) string         { return "mfa:challenge:" + id.String() }
func challengeAttemptsKey(id uuid.UUID) string { return "mfa:challenge:" + id.String() + ":n" }
func reauthKey(sid uuid.UUID) string           { return "auth:reauth:" + sid.String() }

func (s *Service) kv(ctx context.Context, cmds ...valkey.Completed) []valkey.ValkeyResult {
	ctx, cancel := context.WithTimeout(ctx, auth.Timeout)
	defer cancel()
	return s.KV.DoMulti(ctx, cmds...)
}

// startMFA 建立二次验证挑战并返回 401 mfa_required，附 challenge_id 与 methods（AUTH-20）。
// 挑战记录第一步的设备，第二步必须来自同一设备。
func (s *Service) startMFA(ctx context.Context, acct uuid.UUID, in Login) error {
	email, _ := normalize(in.Email)
	c := challenge{Account: acct, Email: email, Platform: in.Device.Platform}
	if in.Device.PublicKey != nil {
		c.PublicKey = *in.Device.PublicKey
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	id := uuid.New()
	if err := s.kv(ctx, s.KV.B().Set().Key(challengeKey(id)).Value(string(b)).Ex(ChallengeTTL).Build())[0].Error(); err != nil {
		return apierr.Unavailable(err)
	}
	_, methods, err := mfa.Enabled(ctx, sqlc.New(s.Pool), acct)
	if err != nil {
		return err
	}
	return apierr.New(401, "mfa_required").With("challenge_id", id).With("methods", methods)
}

// MFALogin 是登录第二步（AUTH-20）：challenge_id、设备，以及 TOTP 码或恢复码之一。
type MFALogin struct {
	ChallengeID            uuid.UUID
	Device                 Device
	TOTPCode, RecoveryCode string
	IP, IPPrefix           string
	UserAgent              string
}

// MFALogin 校验第二步并完成登录。每次尝试预占 AUTH-09 的账号失败名额，成功后清除；每个挑战最多尝试 5 次，用完即作废。
// 挑战不存在、已过期、设备不符或码不正确一律返回 401 unauthenticated。
func (s *Service) MFALogin(ctx context.Context, in MFALogin) (Result, error) {
	if (in.TOTPCode == "") == (in.RecoveryCode == "") {
		return Result{}, apierr.Invalid(apierr.Field("totp_code", "required"))
	}
	pubKey, err := s.validateDevice(in.Device)
	if err != nil {
		return Result{}, err
	}
	res := s.kv(ctx, s.KV.B().Get().Key(challengeKey(in.ChallengeID)).Build())
	raw, err := res[0].AsBytes()
	if valkey.IsValkeyNil(err) {
		return Result{}, apierr.Unauthenticated
	}
	if err != nil {
		return Result{}, apierr.Unavailable(err)
	}
	var c challenge
	if err := json.Unmarshal(raw, &c); err != nil {
		return Result{}, apierr.Unauthenticated
	}
	if err := s.reserveAttempt(ctx, in.IP, c.Email); err != nil {
		return Result{}, err
	}
	pk := ""
	if in.Device.PublicKey != nil {
		pk = *in.Device.PublicKey
	}
	if in.Device.Platform != c.Platform || pk != c.PublicKey {
		return Result{}, apierr.Unauthenticated
	}
	n, err := s.kv(ctx,
		s.KV.B().Incr().Key(challengeAttemptsKey(in.ChallengeID)).Build(),
		s.KV.B().Expire().Key(challengeAttemptsKey(in.ChallengeID)).Seconds(int64(ChallengeTTL/time.Second)).Build(),
	)[0].AsInt64()
	if err != nil {
		return Result{}, apierr.Unavailable(err)
	}
	if n > ChallengeMaxAttempts {
		s.dropChallenge(ctx, in.ChallengeID)
		return Result{}, apierr.Unauthenticated
	}

	var ok bool
	var status string
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		a, err := q.AccountByID(ctx, c.Account)
		if err != nil {
			return err
		}
		// 先检查账号状态：被暂停的账号不消耗恢复码。
		if status = a.Status; status != "active" {
			return nil
		}
		ok, err = s.MFA.Verify(ctx, q, c.Account, in.TOTPCode, in.RecoveryCode)
		return err
	})
	if err == nil && status != "active" {
		s.dropChallenge(ctx, in.ChallengeID)
		return Result{}, apierr.New(403, "account_suspended")
	}
	if err != nil {
		return Result{}, err
	}
	if !ok {
		if n >= ChallengeMaxAttempts {
			s.dropChallenge(ctx, in.ChallengeID)
		}
		return Result{}, apierr.Unauthenticated
	}
	s.dropChallenge(ctx, in.ChallengeID)
	if status != "active" {
		return Result{}, apierr.New(403, "account_suspended")
	}
	if err := s.clearAttempts(ctx, c.Email); err != nil {
		return Result{}, err
	}
	login := Login{Email: c.Email, Device: in.Device, IP: in.IP, IPPrefix: in.IPPrefix, UserAgent: in.UserAgent}
	return s.complete(ctx, c.Account, login, pubKey, []string{"pwd", "otp"})
}

func (s *Service) dropChallenge(ctx context.Context, id uuid.UUID) {
	s.kv(ctx, s.KV.B().Del().Key(challengeKey(id), challengeAttemptsKey(id)).Build())
}

// Reauth 是重新验证的凭据（AUTH-23）：密码、TOTP 码或恢复码之一。
type Reauth struct {
	Password, TOTPCode, RecoveryCode string
	IP                               string
}

// Reauthenticate 重新验证当前会话（AUTH-23），成功后 5 分钟内可以执行需要重新验证的操作。
// 密码或验证码不正确返回 400 incorrect，并计入 AUTH-09 的失败次数。
func (s *Service) Reauthenticate(ctx context.Context, p auth.Principal, in Reauth) (time.Time, error) {
	given := 0
	for _, v := range []string{in.Password, in.TOTPCode, in.RecoveryCode} {
		if v != "" {
			given++
		}
	}
	if given != 1 {
		return time.Time{}, apierr.Invalid(apierr.Field("password", "required"))
	}
	q := sqlc.New(s.Pool)
	a, err := q.AccountByID(ctx, p.AccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, apierr.Unauthenticated
	}
	if err != nil {
		return time.Time{}, err
	}
	email, _ := normalize(a.Email)
	// 重新验证不受登录冷却影响（AUTH-09：冷却期间已登录的会话不受影响，他人无法借此锁死账号），
	// 改为按会话限制尝试次数；失败仍计入账号的失败次数。
	ok, retry, err := s.Limiter.Allow(ctx, ReauthPerSession, p.SessionID.String())
	if err != nil {
		return time.Time{}, apierr.Unavailable(err)
	}
	if !ok {
		return time.Time{}, apierr.RateLimited(retry)
	}
	field := "password"
	switch {
	case in.Password != "":
		if a.PasswordHash != nil {
			if ok, err = s.verify(in.Password, *a.PasswordHash); err != nil {
				return time.Time{}, err
			}
		}
	default:
		field = "totp_code"
		if in.RecoveryCode != "" {
			field = "recovery_code"
		}
		err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			var err error
			ok, err = s.MFA.Verify(ctx, sqlc.New(tx), p.AccountID, in.TOTPCode, in.RecoveryCode)
			return err
		})
		if err != nil {
			return time.Time{}, err
		}
	}
	if !ok {
		if err := s.recordFailure(ctx, email); err != nil {
			return time.Time{}, err
		}
		return time.Time{}, apierr.Invalid(apierr.Field(field, "incorrect"))
	}
	expires := s.Clock.Now().Add(ReauthTTL)
	v := strconv.FormatInt(expires.Unix(), 10)
	if err := s.kv(ctx, s.KV.B().Set().Key(reauthKey(p.SessionID)).Value(v).Ex(ReauthTTL).Build())[0].Error(); err != nil {
		return time.Time{}, apierr.Unavailable(err)
	}
	return expires, nil
}

// RequireRecentAuth 要求当前会话 5 分钟内完成过重新验证，否则返回 401 mfa_required（AUTH-23）。
// methods 只列出二次验证方式；密码总是可用，不列入（spec/30）。
func (s *Service) RequireRecentAuth(ctx context.Context, p auth.Principal) error {
	raw, err := s.kv(ctx, s.KV.B().Get().Key(reauthKey(p.SessionID)).Build())[0].ToString()
	if err == nil {
		if exp, perr := strconv.ParseInt(raw, 10, 64); perr == nil && s.Clock.Now().Before(time.Unix(exp, 0)) {
			return nil
		}
	} else if !valkey.IsValkeyNil(err) {
		return apierr.Unavailable(err)
	}
	_, methods, err := mfa.Enabled(ctx, sqlc.New(s.Pool), p.AccountID)
	if err != nil {
		return err
	}
	if methods == nil {
		methods = []string{}
	}
	return apierr.New(401, "mfa_required").With("methods", methods)
}

// ChangePassword 修改密码（需要重新验证，AUTH-23），成功后吊销除当前会话外的全部会话并发送安全通知。
func (s *Service) ChangePassword(ctx context.Context, p auth.Principal, newPassword string, params password.Params) error {
	if err := s.RequireRecentAuth(ctx, p); err != nil {
		return err
	}
	if err := password.CheckLength(newPassword); err != nil {
		code := "too_long"
		if len([]rune(newPassword)) < password.MinLength {
			code = "too_short"
		}
		return apierr.Invalid(apierr.Field("new_password", code))
	}
	hash, err := password.Hash(newPassword, params)
	if err != nil {
		return err
	}
	var revoked []uuid.UUID
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		a, err := q.AccountByID(ctx, p.AccountID)
		if err != nil {
			return err
		}
		if err := q.SetPasswordHash(ctx, sqlc.SetPasswordHashParams{ID: p.AccountID, PasswordHash: &hash}); err != nil {
			return err
		}
		now := s.Clock.Now()
		if revoked, err = q.RevokeOtherSessions(ctx, sqlc.RevokeOtherSessionsParams{AccountID: p.AccountID, Keep: p.SessionID, Now: &now}); err != nil {
			return err
		}
		return s.Outbox.Enqueue(ctx, q, notify.Message{AccountID: p.AccountID, Template: notify.TemplatePasswordChanged, Locale: a.Locale})
	})
	if err != nil {
		return err
	}
	if err := s.revokeAfterCommit(ctx, revoked); err != nil {
		return apierr.Unavailable(err)
	}
	return nil
}
