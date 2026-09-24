// SPDX-License-Identifier: AGPL-3.0-or-later

package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/notify"
	"github.com/akari-project/panel/server/internal/password"
	"github.com/akari-project/panel/server/internal/ratelimit"
)

// 找回密码（AUTH-04）：链接 30 分钟有效；令牌为 32 字节 CSPRNG 随机值（base64url），
// 放在 URL 片段中，只存 SHA-256。
const (
	ResetTTL     = 30 * time.Minute
	purposeReset = "password_reset"
	// ResetPath 是用户中心的重置密码页，相对于 PortalURL。
	ResetPath = "reset-password"
)

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RequestReset 发起找回密码。无论邮箱是否存在都返回成功（AUTH-04）；限流按邮箱与 IP 计数（API-04）。
// 同一账号发起新的请求时，旧链接立即作废。
func (s *Service) RequestReset(ctx context.Context, email string, captchaToken *string, ip string) error {
	e, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	if err := s.captcha(ctx, captchaToken); err != nil {
		return err
	}
	if err := s.limit(ctx, []ratelimit.Rule{SendPerIP}, ip); err != nil {
		return err
	}
	if err := s.limit(ctx, []ratelimit.Rule{SendPerEmail}, e); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		acct, err := q.AccountByEmail(ctx, e)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// 注销中与已删除的账号不发送（AUTH-05）。
		if acct.Status == "deleting" || acct.Status == "deleted" {
			return nil
		}
		now := s.Clock.Now()
		if err := q.InvalidateVerificationCodes(ctx, sqlc.InvalidateVerificationCodesParams{AccountID: &acct.ID, Purpose: purposeReset, Now: &now}); err != nil {
			return err
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		if err := q.InsertVerificationCode(ctx, sqlc.InsertVerificationCodeParams{
			AccountID: &acct.ID, Purpose: purposeReset, CodeHash: tokenHash(token), ExpiresAt: now.Add(ResetTTL),
		}); err != nil {
			return err
		}
		return s.Outbox.Enqueue(ctx, q, notify.Message{
			AccountID: acct.ID, Template: notify.TemplatePasswordReset, Locale: acct.Locale,
			Vars:     map[string]string{"minutes": strconv.Itoa(int(ResetTTL / time.Minute))},
			Secrets:  map[string]string{"link": s.PortalURL + ResetPath + "#token=" + token},
			RetryFor: ResetTTL,
		})
	})
}

// ConfirmReset 用链接中的令牌设置新密码，成功后吊销该账号的全部会话（AUTH-04）。
func (s *Service) ConfirmReset(ctx context.Context, token, newPassword string) error {
	if raw, err := base64.RawURLEncoding.DecodeString(token); err != nil || len(raw) != 32 {
		return apierr.Invalid(apierr.Field("token", "invalid_format"))
	}
	if err := checkPassword("new_password", newPassword); err != nil {
		return err
	}
	hash, err := password.Hash(newPassword, s.Password)
	if err != nil {
		return err
	}
	var revoked []uuid.UUID
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		c, err := q.VerificationCodeByHash(ctx, sqlc.VerificationCodeByHashParams{CodeHash: tokenHash(token), Purpose: purposeReset})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (c.ConsumedAt != nil || c.AccountID == nil)) {
			return apierr.Invalid(apierr.Field("token", "invalid_code"))
		}
		if err != nil {
			return err
		}
		now := s.Clock.Now()
		if !now.Before(c.ExpiresAt) {
			return apierr.Invalid(apierr.Field("token", "expired"))
		}
		if err := q.ConsumeVerificationCode(ctx, sqlc.ConsumeVerificationCodeParams{ID: c.ID, Now: &now}); err != nil {
			return err
		}
		if err := q.SetPasswordHash(ctx, sqlc.SetPasswordHashParams{ID: *c.AccountID, PasswordHash: &hash}); err != nil {
			return err
		}
		if s.Revoke != nil {
			revoked, err = s.Revoke(ctx, tx, *c.AccountID)
		}
		return err
	})
	if err != nil {
		return err
	}
	if s.AfterRevoke != nil {
		return s.AfterRevoke(ctx, revoked)
	}
	return nil
}
