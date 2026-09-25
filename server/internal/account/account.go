// SPDX-License-Identifier: AGPL-3.0-or-later

// Package account 是账号的注册、邮箱验证与找回密码（spec/10 10.1）。
//
// 防枚举（AUTH-01、AUTH-04）：注册、重新发送验证码、找回密码对已注册与未注册的邮箱返回相同的响应，
// 限流也按邮箱字符串计数而不是按账号，使响应与 429 都不泄露邮箱是否已注册。
package account

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // 校验 IANA 时区名不依赖系统时区数据库

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/clientconfig"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/notify"
	"github.com/akari-project/panel/server/internal/password"
	"github.com/akari-project/panel/server/internal/ratelimit"
	"github.com/akari-project/panel/server/internal/secretbox"
)

// 限流（spec/30 API-04：找回密码与验证码发送每个邮箱每小时 3 次、每个 IP 每小时 10 次；
// spec/10 AUTH-03：重新发送每分钟 1 次、每天 10 次）。按邮箱的计数按用途分开，
// 以免他人反复注册占满受害者找回密码的配额。
var (
	RegisterPerEmail = ratelimit.Rule{Name: "send-register", Limit: 3, Window: time.Hour}
	ResendPerEmail   = ratelimit.Rule{Name: "send-verify", Limit: 3, Window: time.Hour}
	ResetPerEmail    = ratelimit.Rule{Name: "send-reset", Limit: 3, Window: time.Hour}
	SendPerIP        = ratelimit.Rule{Name: "send-ip", Limit: 10, Window: time.Hour}
	ResendPerMin     = ratelimit.Rule{Name: "resend-minute", Limit: 1, Window: time.Minute}
	ResendPerDay     = ratelimit.Rule{Name: "resend-day", Limit: 10, Window: 24 * time.Hour}
)

// InviteResolver 解析邀请码（AUTH-02）：账号邀请码，以及管理员生成的注册码（M1-07 接入）。
// 有效时返回邀请人账号 ID（注册码没有邀请人时为 nil）。
type InviteResolver interface {
	Resolve(ctx context.Context, q *sqlc.Queries, code string) (referrer *uuid.UUID, ok bool, err error)
}

// ReferralCodes 只接受账号邀请码。
type ReferralCodes struct{}

// Resolve 查找账号邀请码。邀请码不区分大小写。
func (ReferralCodes) Resolve(ctx context.Context, q *sqlc.Queries, code string) (*uuid.UUID, bool, error) {
	id, err := q.AccountByReferralCode(ctx, strings.ToUpper(strings.TrimSpace(code)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &id, true, nil
}

// CaptchaVerifier 是人机验证（AUTH-02 预留）。默认实现总是通过。
type CaptchaVerifier interface {
	Verify(ctx context.Context, token string) (bool, error)
}

// NoCaptcha 总是通过。
type NoCaptcha struct{}

// Verify 总是返回 true。
func (NoCaptcha) Verify(context.Context, string) (bool, error) { return true, nil }

// Service 实现注册、邮箱验证与找回密码。
type Service struct {
	Pool     *pgxpool.Pool
	Clock    clock.Clock
	Keys     *secretbox.Keyring
	Outbox   notify.Outbox
	Limiter  ratelimit.Limiter
	Password password.Params
	Invites  InviteResolver
	Captcha  CaptchaVerifier
	// PortalURL 是用户中心的绝对地址（以 / 结尾），用于找回密码链接。取自配置，
	// 不取自请求的 Host，以免链接被伪造的 Host 指向他处。
	PortalURL string
	// Revoke 吊销账号的全部会话（找回密码成功后，AUTH-04），由会话服务提供。
	Revoke func(ctx context.Context, tx pgx.Tx, account uuid.UUID) ([]uuid.UUID, error)
	// AfterRevoke 在事务提交后把会话写入吊销集合。
	AfterRevoke func(ctx context.Context, sids []uuid.UUID) error
	// Log 记录 settings 值异常的告警（spec/03 3.6）；为 nil 时不记录。
	Log *slog.Logger

	warn clientconfig.SettingsWarner
}

// Registration 是注册请求。
type Registration struct {
	Email, Password string
	InviteCode      *string
	Locale          string
	Timezone        *string
	CaptchaToken    *string
	IP              string
}

// NormalizeEmail 校验邮箱并按小写归一化（AUTH-01）。
func NormalizeEmail(s string) (string, error) {
	if len(s) > 254 {
		return "", apierr.Invalid(apierr.Field("email", "too_long"))
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != strings.TrimSpace(s) || !strings.Contains(a.Address, "@") {
		return "", apierr.Invalid(apierr.Field("email", "invalid_format"))
	}
	return strings.ToLower(a.Address), nil
}

func checkPassword(field, pw string) error {
	if err := password.CheckLength(pw); err != nil {
		if len([]rune(pw)) < password.MinLength {
			return apierr.Invalid(apierr.Field(field, "too_short"))
		}
		return apierr.Invalid(apierr.Field(field, "too_long"))
	}
	return nil
}

var localeRE = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$`)

// ValidLocale 报告 l 是否是合法的 BCP 47 语言标签（宽松校验）。
func ValidLocale(l string) bool { return len(l) <= 35 && localeRE.MatchString(l) }

// RegistrationControl 返回有效的注册控制（spec/10 AUTH-02、spec/03 3.6）：缺键按 open 与空名单；
// 三个注册控制键中任一值异常时策略为 closed，并记 warn 日志，只记键名（CONV-24），同一份异常值每个进程
// 只告警一次；告警记录在 Log 为 nil 时同样更新，只是不输出。三个键在一条语句中读取，策略与名单来自同一个
// 快照。q 由调用方提供，使 GET /v1/config 与其他设置在同一个 q 上读取。
func (s *Service) RegistrationControl(ctx context.Context, q *sqlc.Queries) (clientconfig.RegistrationControl, error) {
	keys := [3]string{"registration_policy", "email_domain_allowlist", "email_domain_denylist"}
	rows, err := q.GetSettings(ctx, keys[:])
	if err != nil {
		return clientconfig.RegistrationControl{}, err
	}
	var raw [3][]byte
	for _, r := range rows {
		raw[slices.Index(keys[:], r.Key)] = r.Value
	}
	rc, invalid := clientconfig.Registration(raw[0], raw[1], raw[2])
	for i, k := range keys {
		if s.warn.Changed(k, raw[i], slices.Contains(invalid, k)) && s.Log != nil {
			s.Log.WarnContext(ctx, "settings: invalid value, registration closed", "key", k)
		}
	}
	return rc, nil
}

// checkDomain 按黑白名单检查邮箱域名（AUTH-02）：在黑名单中即拒绝；白名单非空时必须在白名单中。
func checkDomain(rc clientconfig.RegistrationControl, email string) error {
	domain := email[strings.LastIndex(email, "@")+1:]
	in := func(list []string) bool {
		return slices.ContainsFunc(list, func(d string) bool { return strings.EqualFold(strings.TrimSpace(d), domain) })
	}
	if in(rc.Deny) || (len(rc.Allow) > 0 && !in(rc.Allow)) {
		return apierr.Invalid(apierr.Field("email", "not_allowed"))
	}
	return nil
}

func (s *Service) limit(ctx context.Context, rules []ratelimit.Rule, subject string) error {
	for _, r := range rules {
		ok, retry, err := s.Limiter.Allow(ctx, r, subject)
		if err != nil {
			return apierr.Unavailable(err)
		}
		if !ok {
			return apierr.RateLimited(retry)
		}
	}
	return nil
}

func (s *Service) captcha(ctx context.Context, token *string) error {
	t := ""
	if token != nil {
		t = *token
	}
	ok, err := s.Captcha.Verify(ctx, t)
	if err != nil {
		return apierr.Unavailable(err)
	}
	if !ok {
		return apierr.Invalid(apierr.Field("captcha_token", "invalid_code"))
	}
	return nil
}

// Register 注册账号（AUTH-01、AUTH-02、AUTH-03、AUTH-13）。成功与邮箱已注册返回相同结果：
// 邮箱已注册时不创建账号，改为向该邮箱发送“有人尝试用你的邮箱注册”。
func (s *Service) Register(ctx context.Context, in Registration) error {
	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return err
	}
	if err := checkPassword("password", in.Password); err != nil {
		return err
	}
	if in.InviteCode != nil && len(*in.InviteCode) > 64 {
		return apierr.Invalid(apierr.Field("invite_code", "too_long"))
	}
	if in.Timezone != nil {
		if _, err := time.LoadLocation(*in.Timezone); err != nil || *in.Timezone == "" || *in.Timezone == "Local" {
			return apierr.Invalid(apierr.Field("timezone", "invalid_format"))
		}
	}
	if err := s.captcha(ctx, in.CaptchaToken); err != nil {
		return err
	}
	q := sqlc.New(s.Pool)
	rc, err := s.RegistrationControl(ctx, q)
	if err != nil {
		return err
	}
	hasInvite := in.InviteCode != nil && strings.TrimSpace(*in.InviteCode) != ""
	if rc.Policy == "closed" || (rc.Policy == "invite_only" && !hasInvite) {
		return apierr.New(403, "registration_closed")
	}
	if err := checkDomain(rc, email); err != nil {
		return err
	}
	var referrer *uuid.UUID
	if hasInvite {
		r, ok, err := s.Invites.Resolve(ctx, q, *in.InviteCode)
		if err != nil {
			return err
		}
		if !ok {
			return apierr.Invalid(apierr.Field("invite_code", "invalid_code"))
		}
		referrer = r
	}
	if err := s.limit(ctx, []ratelimit.Rule{SendPerIP}, in.IP); err != nil {
		return err
	}
	if err := s.limit(ctx, []ratelimit.Rule{RegisterPerEmail}, email); err != nil {
		return err
	}
	// 先哈希再查询邮箱，已注册与未注册两条路径的耗时相同。
	hash, err := password.Hash(in.Password, s.Password)
	if err != nil {
		return err
	}
	locale := in.Locale
	if !ValidLocale(locale) {
		locale = notify.DefaultLocale
	}

	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		existing, err := q.AccountByEmail(ctx, email)
		if err == nil {
			return s.Outbox.Enqueue(ctx, q, notify.Message{
				AccountID: existing.ID, Template: notify.TemplateRegisterAttempt, Locale: existing.Locale,
			})
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		code, err := UniqueReferralCode(ctx, q)
		if err != nil {
			return err
		}
		id, err := q.RegisterAccount(ctx, sqlc.RegisterAccountParams{
			Email: email, PasswordHash: &hash, ReferralCode: code, ReferrerID: referrer, Locale: locale, Timezone: in.Timezone,
		})
		if err != nil {
			return err
		}
		if _, err := CreateSharedCredential(ctx, q, s.Keys, id); err != nil {
			return err
		}
		return s.sendVerificationCode(ctx, q, id, locale)
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.ConstraintName == "accounts_email_uq" {
		// 并发注册同一邮箱：后到者按“邮箱已注册”处理，响应相同。
		return nil
	}
	return err
}

// randomDigits 返回 n 位十进制随机数字（CSPRNG，均匀分布）。
func randomDigits(n int) string {
	b := make([]byte, n)
	buf := make([]byte, 1)
	for i := 0; i < n; {
		if _, err := rand.Read(buf); err != nil {
			panic(fmt.Sprintf("account: crypto/rand: %v", err))
		}
		if buf[0] >= 250 { // 拒绝采样：250 = 25 × 10
			continue
		}
		b[i] = '0' + buf[0]%10
		i++
	}
	return string(b)
}
