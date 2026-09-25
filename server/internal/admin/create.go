// SPDX-License-Identifier: AGPL-3.0-or-later

// Package admin 实现 `panel admin create`：创建首个超级管理员（spec/10 AUTH-21）。
package admin

import (
	"context"
	"errors"
	"net/mail"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/akari-project/panel/server/internal/account"
	"github.com/akari-project/panel/server/internal/audit"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/password"
	"github.com/akari-project/panel/server/internal/secretbox"
)

// SuperadminRole 是内置超级管理员角色。
const SuperadminRole = "superadmin"

// 错误。
var (
	ErrInvalidEmail     = errors.New("admin: invalid email address")
	ErrEmailTaken       = errors.New("admin: an account with this email already exists")
	ErrSuperadminExists = errors.New("admin: a superadmin already exists; add further staff through invitations (spec/10 AUTH-22)")
)

// Creator 创建首个超级管理员。
type Creator struct {
	Pool     *pgxpool.Pool
	Clock    clock.Clock
	Keys     *secretbox.Keyring
	Password password.Params
}

// Result 是创建结果。
type Result struct {
	AccountID    uuid.UUID
	CredentialID uuid.UUID
}

// Create 在一个事务中创建账号、授予 superadmin、生成共用代理凭据（AUTH-13）、
// 写入 credential.changed 事件（CONV-22）与审计日志（AUTH-18）。
// 账号首次登录时必须绑定 TOTP（AUTH-21），因此这里不创建二次验证。
func (c *Creator) Create(ctx context.Context, email, pw string) (Result, error) {
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != strings.TrimSpace(email) || addr.Name != "" {
		return Result{}, ErrInvalidEmail
	}
	email = strings.ToLower(addr.Address)
	if err := password.CheckLength(pw); err != nil {
		return Result{}, err
	}
	hash, err := password.Hash(pw, c.Password)
	if err != nil {
		return Result{}, err
	}

	var res Result
	err = pgx.BeginFunc(ctx, c.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		// 串行化并发的 admin create，保证“首个”超级管理员只有一个。
		if _, err := tx.Exec(ctx, `LOCK TABLE account_roles IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		n, err := q.CountRoleMembers(ctx, SuperadminRole)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrSuperadminExists
		}
		taken, err := q.AccountEmailExists(ctx, email)
		if err != nil {
			return err
		}
		if taken {
			return ErrEmailTaken
		}
		code, err := account.UniqueReferralCode(ctx, q)
		if err != nil {
			return err
		}
		now := c.Clock.Now()
		res.AccountID, err = q.CreateAccount(ctx, sqlc.CreateAccountParams{
			Email:           email,
			PasswordHash:    &hash,
			EmailVerifiedAt: &now,
			ReferralCode:    code,
		})
		if err != nil {
			return err
		}
		if err := q.GrantRole(ctx, sqlc.GrantRoleParams{AccountID: res.AccountID, Role: SuperadminRole}); err != nil {
			return err
		}
		// 共用凭据与 credential.changed 事件（AUTH-13、CONV-34）。
		res.CredentialID, err = account.CreateSharedCredential(ctx, q, c.Keys, res.AccountID)
		if err != nil {
			return err
		}
		// 审计记录不含邮箱明文；原因写入 reason_texts，审计日志只引用其 ID（CONV-29）。
		// 没有请求，request_id 为生成的 UUIDv7，操作者为空（AUTH-18）。
		return audit.Record(ctx, q, audit.Entry{
			Action: "staff.create", TargetType: "account", TargetID: res.AccountID.String(),
			Diff:   audit.Values(map[string]any{"roles": []string{SuperadminRole}}),
			Reason: "panel admin create", ReasonAccount: &res.AccountID,
		})
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}
