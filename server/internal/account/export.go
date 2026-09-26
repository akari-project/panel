// SPDX-License-Identifier: AGPL-3.0-or-later

package account

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/secretbox"
)

// ExportTokenAD 是 export_tokens.token_enc 的附加数据。
var ExportTokenAD = []byte("export_tokens.token_enc")

// ExportToken 是账号当前的导出令牌明文与最近一次生成或重置的时刻（AUTH-16）。
type ExportToken struct {
	Token     string
	RotatedAt time.Time
}

// newExportToken 生成 32 字节 CSPRNG 随机值（base64url），返回明文、哈希与密文（CONV-19、CONV-20 例外）。
func newExportToken(keys *secretbox.Keyring) (plain, hash string, enc []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", nil, err
	}
	plain = base64.RawURLEncoding.EncodeToString(b)
	enc, err = keys.Seal([]byte(plain), ExportTokenAD)
	return plain, tokenHash(plain), enc, err
}

// CreateExportToken 在账号创建的事务中生成导出令牌（与共用凭据同时，AUTH-13、AUTH-16）。已有令牌时不变。
func CreateExportToken(ctx context.Context, q *sqlc.Queries, keys *secretbox.Keyring, acct uuid.UUID, now time.Time) error {
	_, hash, enc, err := newExportToken(keys)
	if err != nil {
		return err
	}
	return q.InsertExportToken(ctx, sqlc.InsertExportTokenParams{AccountID: acct, TokenHash: hash, TokenEnc: enc, Now: now})
}

// ExportToken 返回账号当前的导出令牌（GET /v1/me/export-link）。没有令牌时（AUTH-22 的凭据重置会删除）补建：
// 并发的补建只有一条生效，之后重新读取。只为正常或暂停的账号补建；正在注销与已注销的账号返回 404。
func (s *Service) ExportToken(ctx context.Context, acct uuid.UUID) (ExportToken, error) {
	q := sqlc.New(s.Pool)
	row, err := q.ExportToken(ctx, acct)
	if errors.Is(err, pgx.ErrNoRows) {
		_, hash, enc, nerr := newExportToken(s.Keys)
		if nerr != nil {
			return ExportToken{}, nerr
		}
		if err := q.BackfillExportToken(ctx, sqlc.BackfillExportTokenParams{AccountID: acct, TokenHash: hash, TokenEnc: enc, Now: s.Clock.Now()}); err != nil {
			return ExportToken{}, err
		}
		row, err = q.ExportToken(ctx, acct)
		if errors.Is(err, pgx.ErrNoRows) {
			return ExportToken{}, apierr.NotFound
		}
	}
	if err != nil {
		return ExportToken{}, err
	}
	pt, err := s.Keys.Open(row.TokenEnc, ExportTokenAD)
	if err != nil {
		return ExportToken{}, err
	}
	return ExportToken{Token: string(pt), RotatedAt: row.RotatedAt}, nil
}

// RotateExportToken 重置导出令牌（AUTH-16，调用方已要求重新验证，AUTH-23）：在同一事务中生成新令牌（旧链接立即失效），
// 吊销共用凭据（credential.changed revoked）并生成新的共用凭据（rotated），与 AUTH-22 的凭据重置写同样的事件。
func (s *Service) RotateExportToken(ctx context.Context, acct uuid.UUID) (ExportToken, error) {
	var out ExportToken
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		now := s.Clock.Now()
		if _, err := q.LockAccount(ctx, acct); err != nil {
			return err
		}
		// 正在注销与已注销的账号不再生成令牌与共用凭据（AUTH-05）。
		if a, err := q.AccountByID(ctx, acct); err != nil {
			return err
		} else if a.Status != "active" && a.Status != "suspended" {
			return apierr.InvalidState
		}
		plain, hash, enc, err := newExportToken(s.Keys)
		if err != nil {
			return err
		}
		if err := q.ReplaceExportToken(ctx, sqlc.ReplaceExportTokenParams{AccountID: acct, TokenHash: hash, TokenEnc: enc, Now: now}); err != nil {
			return err
		}
		old, err := q.RevokeSharedCredential(ctx, sqlc.RevokeSharedCredentialParams{AccountID: acct, Now: &now})
		if err != nil {
			return err
		}
		for _, c := range old {
			if err := CredentialChanged(ctx, q, acct, c, "revoked"); err != nil {
				return err
			}
		}
		if _, err := RotateSharedCredential(ctx, q, s.Keys, acct); err != nil {
			return err
		}
		out = ExportToken{Token: plain, RotatedAt: now}
		return nil
	})
	return out, err
}
