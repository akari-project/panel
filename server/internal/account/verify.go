// SPDX-License-Identifier: AGPL-3.0-or-later

package account

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/notify"
	"github.com/akari-project/panel/server/internal/ratelimit"
)

// 邮箱验证码（AUTH-03）：6 位数字，15 分钟有效，最多尝试 5 次。
const (
	CodeTTL         = 15 * time.Minute
	CodeMaxAttempts = 5
	purposeVerify   = "email_verify"
)

// codeHash 是验证码的 SHA-256（CONV-20）。账号 ID 参与哈希，同一个码在不同账号下的哈希不同。
func codeHash(account uuid.UUID, code string) string {
	sum := sha256.Sum256([]byte(account.String() + ":" + code))
	return hex.EncodeToString(sum[:])
}

// sendVerificationCode 生成新验证码，作废旧码，并在同一事务中写入通知队列。
func (s *Service) sendVerificationCode(ctx context.Context, q *sqlc.Queries, account uuid.UUID, locale string) error {
	now := s.Clock.Now()
	if err := q.InvalidateVerificationCodes(ctx, sqlc.InvalidateVerificationCodesParams{AccountID: &account, Purpose: purposeVerify, Now: &now}); err != nil {
		return err
	}
	code := randomDigits(6)
	if err := q.InsertVerificationCode(ctx, sqlc.InsertVerificationCodeParams{
		AccountID: &account, Purpose: purposeVerify, CodeHash: codeHash(account, code), ExpiresAt: now.Add(CodeTTL),
	}); err != nil {
		return err
	}
	return s.Outbox.Enqueue(ctx, q, notify.Message{
		AccountID: account, Template: notify.TemplateEmailVerification, Locale: locale,
		Vars:     map[string]string{"minutes": strconv.Itoa(int(CodeTTL / time.Minute))},
		Secrets:  map[string]string{"code": code},
		RetryFor: CodeTTL,
	})
}

// target 确定验证码针对的账号：已登录时为本人；未登录时按邮箱查找。
// 未登录且邮箱未注册时 found 为 false，由调用方给出与“码错误”相同的响应。
func (s *Service) target(ctx context.Context, q *sqlc.Queries, account *uuid.UUID, email *string) (row sqlc.AccountByEmailRow, found bool, err error) {
	if account != nil {
		r, err := q.AccountByID(ctx, *account)
		if errors.Is(err, pgx.ErrNoRows) {
			return row, false, apierr.Unauthenticated
		}
		return sqlc.AccountByEmailRow(r), err == nil, err
	}
	if email == nil {
		return row, false, apierr.Invalid(apierr.Field("email", "required"))
	}
	e, err := NormalizeEmail(*email)
	if err != nil {
		return row, false, err
	}
	row, err = q.AccountByEmail(ctx, e)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, false, nil
	}
	return row, err == nil, err
}

// VerifyEmail 校验邮箱验证码（AUTH-03）。account 为已登录的账号，未登录时为 nil 并提供 email。
func (s *Service) VerifyEmail(ctx context.Context, account *uuid.UUID, email *string, code string) error {
	if len(code) != 6 || !allDigits(code) {
		return apierr.Invalid(apierr.Field("code", "invalid_format"))
	}
	invalid := apierr.Invalid(apierr.Field("code", "invalid_code"))
	// 尝试次数的更新必须在错误时也提交，因此不以错误结束事务，而是把结果带出事务。
	var result error
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		acct, found, err := s.target(ctx, q, account, email)
		if err != nil {
			return err
		}
		if !found {
			result = invalid
			return nil
		}
		if acct.EmailVerifiedAt != nil {
			if account != nil {
				result = apierr.InvalidState
			} else {
				result = invalid // 未登录时不透露邮箱是否已验证
			}
			return nil
		}
		c, err := q.ActiveVerificationCode(ctx, sqlc.ActiveVerificationCodeParams{AccountID: &acct.ID, Purpose: purposeVerify})
		if errors.Is(err, pgx.ErrNoRows) {
			result = invalid
			return nil
		}
		if err != nil {
			return err
		}
		now := s.Clock.Now()
		switch {
		case !now.Before(c.ExpiresAt):
			result = apierr.Invalid(apierr.Field("code", "expired"))
			return nil
		case c.Attempts >= CodeMaxAttempts:
			result = apierr.Invalid(apierr.Field("code", "exhausted"))
			return nil
		}
		if subtle.ConstantTimeCompare([]byte(c.CodeHash), []byte(codeHash(acct.ID, code))) != 1 {
			n, err := q.IncrementCodeAttempts(ctx, c.ID)
			if err != nil {
				return err
			}
			if n >= CodeMaxAttempts {
				result = apierr.Invalid(apierr.Field("code", "exhausted"))
			} else {
				result = invalid
			}
			return nil
		}
		if err := q.ConsumeVerificationCode(ctx, sqlc.ConsumeVerificationCodeParams{ID: c.ID, Now: &now}); err != nil {
			return err
		}
		return q.MarkEmailVerified(ctx, sqlc.MarkEmailVerifiedParams{ID: acct.ID, Now: &now})
	})
	if err != nil {
		return err
	}
	return result
}

// ResendVerification 重新发送验证码（AUTH-03）。未登录且邮箱未注册或已验证时同样返回成功，但不发送。
// 限流按邮箱与 IP 计数（API-04、AUTH-03），与账号是否存在无关。
func (s *Service) ResendVerification(ctx context.Context, account *uuid.UUID, email *string, captchaToken *string, ip string) error {
	if account == nil {
		if err := s.captcha(ctx, captchaToken); err != nil {
			return err
		}
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		acct, found, err := s.target(ctx, q, account, email)
		if err != nil {
			return err
		}
		if account != nil && acct.EmailVerifiedAt != nil {
			return apierr.InvalidState
		}
		subject := ""
		if email != nil {
			subject, _ = NormalizeEmail(*email)
		}
		if account != nil {
			subject = acct.Email
		}
		if err := s.limit(ctx, []ratelimit.Rule{SendPerIP}, ip); err != nil {
			return err
		}
		if err := s.limit(ctx, []ratelimit.Rule{ResendPerMin, ResendPerDay, SendPerEmail}, subject); err != nil {
			return err
		}
		if !found || acct.EmailVerifiedAt != nil {
			return nil
		}
		return s.sendVerificationCode(ctx, q, acct.ID, acct.Locale)
	})
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ErrEmailUnverified 是未验证邮箱的账号下单时的错误（spec/02 错误表，AUTH-03）。
var ErrEmailUnverified = apierr.New(403, "email_unverified")

// RequireVerifiedEmail 要求账号已验证邮箱。下单（M1-06）在同一事务中调用。
func RequireVerifiedEmail(ctx context.Context, q *sqlc.Queries, account uuid.UUID) error {
	a, err := q.AccountByID(ctx, account)
	if errors.Is(err, pgx.ErrNoRows) {
		return apierr.NotFound
	}
	if err != nil {
		return err
	}
	if a.EmailVerifiedAt == nil {
		return ErrEmailUnverified
	}
	return nil
}
