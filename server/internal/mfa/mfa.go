// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mfa 是二次验证（spec/10 10.3）：TOTP 的绑定、启用、停用与恢复码（AUTH-11），
// 以及登录第二步与重新验证共用的校验（AUTH-20、AUTH-23）。Passkey 在 M4 实现。
package mfa

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/notify"
	"github.com/akari-project/panel/server/internal/secretbox"
)

// 恢复码（AUTH-11）：启用时生成 10 个，用后即作废，剩余不足 3 个时发送安全通知。
const (
	RecoveryCodes   = 10
	RecoveryLowMark = 3
	// EnrollmentTTL 是待确认密钥的有效期。
	EnrollmentTTL = 10 * time.Minute
)

var (
	secretAD     = []byte("mfa_totp.secret_enc")
	enrollmentAD = []byte("mfa.enrollment")
)

// Service 管理账号的二次验证。
type Service struct {
	Pool   *pgxpool.Pool
	KV     valkey.Client
	Clock  clock.Clock
	Keys   *secretbox.Keyring
	Outbox notify.Outbox
	// Issuer 是验证器应用中显示的站点名。
	Issuer string
	// AfterRevoke 把事务中吊销的会话写入吊销集合（AUTH-21：二次验证变化时吊销管理会话）。
	AfterRevoke func(ctx context.Context, sids []uuid.UUID) error
}

func enrollmentKey(account uuid.UUID) string { return "mfa:enroll:" + account.String() }

// Enrollment 是待确认的 TOTP 密钥。
type Enrollment struct {
	Secret, URI string
	ExpiresAt   time.Time
}

// StartEnrollment 生成待确认的密钥（spec/30 POST /v1/me/mfa/totp）。已启用时返回 409 invalid_state。
// 密钥加密后保存在 Valkey（CONV-19），确认后才写入数据库。
func (s *Service) StartEnrollment(ctx context.Context, account uuid.UUID) (Enrollment, error) {
	q := sqlc.New(s.Pool)
	if _, err := q.GetTotp(ctx, account); err == nil {
		return Enrollment{}, apierr.InvalidState
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Enrollment{}, err
	}
	a, err := q.AccountByID(ctx, account)
	if err != nil {
		return Enrollment{}, err
	}
	secret := make([]byte, SecretSize)
	if _, err := rand.Read(secret); err != nil {
		return Enrollment{}, err
	}
	enc, err := s.Keys.Seal(secret, enrollmentAD)
	if err != nil {
		return Enrollment{}, err
	}
	if err := s.kvDo(ctx, s.KV.B().Set().Key(enrollmentKey(account)).Value(string(enc)).Ex(EnrollmentTTL).Build()); err != nil {
		return Enrollment{}, apierr.Unavailable(err)
	}
	return Enrollment{Secret: EncodeSecret(secret), URI: URI(s.Issuer, a.Email, secret), ExpiresAt: s.Clock.Now().Add(EnrollmentTTL)}, nil
}

func (s *Service) kvDo(ctx context.Context, cmd valkey.Completed) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.KV.Do(ctx, cmd).Error()
}

// Activate 用一次验证码确认绑定，生成恢复码（AUTH-11）。恢复码明文只在返回值中出现一次。
func (s *Service) Activate(ctx context.Context, account uuid.UUID, code string) ([]string, error) {
	if len(code) != Digits {
		return nil, apierr.Invalid(apierr.Field("totp_code", "invalid_format"))
	}
	kctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	raw, err := s.KV.Do(kctx, s.KV.B().Get().Key(enrollmentKey(account)).Build()).AsBytes()
	cancel()
	if valkey.IsValkeyNil(err) {
		return nil, apierr.InvalidState // 没有进行中的绑定，或已过期
	}
	if err != nil {
		return nil, apierr.Unavailable(err)
	}
	secret, err := s.Keys.Open(raw, enrollmentAD)
	if err != nil {
		return nil, apierr.InvalidState
	}
	step, ok := Match(secret, code, s.Clock.Now(), nil)
	if !ok {
		return nil, apierr.Invalid(apierr.Field("totp_code", "incorrect"))
	}
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	enc, err := s.Keys.Seal(secret, secretAD)
	if err != nil {
		return nil, err
	}
	var revoked []uuid.UUID
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetTotp(ctx, account); err == nil {
			return apierr.InvalidState
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		now := s.Clock.Now()
		if err := q.InsertTotp(ctx, sqlc.InsertTotpParams{
			AccountID: account, SecretEnc: enc, RecoveryHashes: hashes, LastUsedStep: &step, EnabledAt: &now,
		}); err != nil {
			return err
		}
		if revoked, err = q.RevokeConsoleSessions(ctx, sqlc.RevokeConsoleSessionsParams{AccountID: account, Now: &now}); err != nil {
			return err
		}
		return s.notify(ctx, q, account, notify.TemplateMFAEnabled, nil)
	})
	if err != nil {
		return nil, err
	}
	_ = s.kvDo(ctx, s.KV.B().Del().Key(enrollmentKey(account)).Build())
	return codes, s.afterRevoke(ctx, revoked)
}

// Disable 停用 TOTP（调用方已检查重新验证，AUTH-23）。管理员不能停用（AUTH-12）。
func (s *Service) Disable(ctx context.Context, account uuid.UUID) error {
	var revoked []uuid.UUID
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetTotp(ctx, account); errors.Is(err, pgx.ErrNoRows) {
			return apierr.InvalidState
		} else if err != nil {
			return err
		}
		staff, err := q.HasStaffRole(ctx, account)
		if err != nil {
			return err
		}
		if staff {
			return apierr.InvalidState
		}
		if err := q.DeleteTotp(ctx, account); err != nil {
			return err
		}
		now := s.Clock.Now()
		if revoked, err = q.RevokeConsoleSessions(ctx, sqlc.RevokeConsoleSessionsParams{AccountID: account, Now: &now}); err != nil {
			return err
		}
		return s.notify(ctx, q, account, notify.TemplateMFADisabled, nil)
	})
	if err != nil {
		return err
	}
	return s.afterRevoke(ctx, revoked)
}

// RegenerateRecoveryCodes 重新生成恢复码，旧恢复码立即作废（调用方已检查重新验证，AUTH-23）。
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, account uuid.UUID) ([]string, error) {
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetTotp(ctx, account); errors.Is(err, pgx.ErrNoRows) {
			return apierr.InvalidState
		} else if err != nil {
			return err
		}
		return q.SetRecoveryHashes(ctx, sqlc.SetRecoveryHashesParams{AccountID: account, RecoveryHashes: hashes})
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

func (s *Service) afterRevoke(ctx context.Context, sids []uuid.UUID) error {
	if len(sids) == 0 || s.AfterRevoke == nil {
		return nil
	}
	if err := s.AfterRevoke(ctx, sids); err != nil {
		return apierr.Unavailable(err)
	}
	return nil
}

func (s *Service) notify(ctx context.Context, q *sqlc.Queries, account uuid.UUID, template string, vars map[string]string) error {
	a, err := q.AccountByID(ctx, account)
	if err != nil {
		return err
	}
	return s.Outbox.Enqueue(ctx, q, notify.Message{AccountID: account, Template: template, Locale: a.Locale, Vars: vars})
}

// Enabled 报告账号是否启用了 TOTP，以及可用于二次验证的方式（AUTH-20 的 methods）。
func Enabled(ctx context.Context, q *sqlc.Queries, account uuid.UUID) (bool, []string, error) {
	_, err := q.GetTotp(ctx, account)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	return true, []string{"totp", "recovery_code"}, nil
}

// Verify 在调用方的事务中校验 TOTP 码或恢复码（二者之一）。成功时记录已使用的时间步或作废恢复码；
// 恢复码剩余不足 3 个时写入安全通知。未启用 TOTP 或码不正确时 ok 为 false。
func (s *Service) Verify(ctx context.Context, q *sqlc.Queries, account uuid.UUID, totpCode, recoveryCode string) (bool, error) {
	t, err := q.GetTotp(ctx, account)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch {
	case totpCode != "":
		secret, err := s.Keys.Open(t.SecretEnc, secretAD)
		if err != nil {
			return false, err
		}
		step, ok := Match(secret, totpCode, s.Clock.Now(), t.LastUsedStep)
		if !ok {
			return false, nil
		}
		return true, q.SetTotpStep(ctx, sqlc.SetTotpStepParams{AccountID: account, Step: &step})
	case recoveryCode != "":
		h := recoveryHash(recoveryCode)
		idx := -1
		for i, x := range t.RecoveryHashes {
			if x == h {
				idx = i
				break
			}
		}
		if idx < 0 {
			return false, nil
		}
		rest := append(append([]string{}, t.RecoveryHashes[:idx]...), t.RecoveryHashes[idx+1:]...)
		if err := q.SetRecoveryHashes(ctx, sqlc.SetRecoveryHashesParams{AccountID: account, RecoveryHashes: rest}); err != nil {
			return false, err
		}
		if len(rest) < RecoveryLowMark {
			if err := s.notify(ctx, q, account, notify.TemplateRecoveryCodesLow, map[string]string{"remaining": strconv.Itoa(len(rest))}); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	return false, nil
}

// 恢复码字母表：小写字母与数字，去除易混字符。
const recoveryAlphabet = "23456789abcdefghjkmnpqrstuvwxyz"

// newRecoveryCodes 生成 10 个形如 xxxxx-xxxxx 的恢复码及其 SHA-256（CONV-20）。
func newRecoveryCodes() (codes, hashes []string, err error) {
	for range RecoveryCodes {
		b := make([]byte, 10)
		for i := 0; i < len(b); {
			var x [1]byte
			if _, err := rand.Read(x[:]); err != nil {
				return nil, nil, err
			}
			if int(x[0]) >= 256-256%len(recoveryAlphabet) {
				continue
			}
			b[i] = recoveryAlphabet[int(x[0])%len(recoveryAlphabet)]
			i++
		}
		code := string(b[:5]) + "-" + string(b[5:])
		codes = append(codes, code)
		hashes = append(hashes, recoveryHash(code))
	}
	return codes, hashes, nil
}

// recoveryHash 按归一化形式（小写、去掉空白与连字符）计算哈希，用户输入时不区分大小写与分隔。
func recoveryHash(code string) string {
	n := strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == ' ' || r == '\t':
			return -1
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return r
	}, code)
	sum := sha256.Sum256([]byte(n))
	return hex.EncodeToString(sum[:])
}
