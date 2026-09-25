// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

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
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/akari-project/panel/server/internal/account"
	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/audit"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/consoleapi/gen"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/notify"
	"github.com/akari-project/panel/server/internal/password"
	"github.com/akari-project/panel/server/internal/secretbox"
)

// InvitationTTL 是邀请的有效期（AUTH-22）。
const InvitationTTL = 72 * time.Hour

// InvitationPath 是管理后台接受邀请页的路径；令牌放在 URL 片段中（AUTH-22，与 AUTH-04 相同）。
const InvitationPath = "accept-invitation"

func invitationTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type invitationRow struct {
	ID         uuid.UUID
	Email      string
	InviterID  uuid.UUID
	ExpiresAt  time.Time
	AcceptedAt *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
	Roles      []string
}

// status 按列推导邀请状态；过期按注入的时钟判断（CONV-27）。
func (r invitationRow) status(now time.Time) gen.StaffInvitationStatus {
	switch {
	case r.AcceptedAt != nil:
		return gen.StaffInvitationStatusAccepted
	case r.RevokedAt != nil:
		return gen.StaffInvitationStatusRevoked
	case !now.Before(r.ExpiresAt):
		return gen.StaffInvitationStatusExpired
	}
	return gen.StaffInvitationStatusPending
}

func (r invitationRow) api(now time.Time) gen.StaffInvitation {
	return gen.StaffInvitation{Id: r.ID, Email: openapi_types.Email(r.Email), Roles: r.Roles, Status: r.status(now),
		InvitedBy: r.InviterID, ExpiresAt: r.ExpiresAt, CreatedAt: r.CreatedAt}
}

// ListStaffInvitations 列出邀请，按 ID 倒序分页（CONV-11）。
func (s *Server) ListStaffInvitations(ctx context.Context, req gen.ListStaffInvitationsRequestObject) (gen.ListStaffInvitationsResponseObject, error) {
	limit, err := pageLimit(req.Params.Limit)
	if err != nil {
		return nil, err
	}
	var status *string
	if req.Params.Status != nil {
		switch st := *req.Params.Status; st {
		case gen.ListStaffInvitationsParamsStatusPending, gen.ListStaffInvitationsParamsStatusAccepted,
			gen.ListStaffInvitationsParamsStatusExpired, gen.ListStaffInvitationsParamsStatusRevoked:
			v := string(st)
			status = &v
		default:
			return nil, apierr.Invalid(apierr.Field("status", "invalid_format"))
		}
	}
	var cur struct {
		ID uuid.UUID `json:"id"`
	}
	var before *uuid.UUID
	if ok, err := decodeCursor(req.Params.Cursor, &cur); err != nil {
		return nil, err
	} else if ok {
		before = &cur.ID
	}
	now := s.d.Clock.Now()
	rows, err := sqlc.New(s.d.Pool).ListInvitations(ctx, sqlc.ListInvitationsParams{Status: status, Now: now, Before: before, MaxRows: limit + 1})
	if err != nil {
		return nil, err
	}
	var next *string
	if len(rows) > int(limit) {
		rows = rows[:limit]
		next = encodeCursor(map[string]any{"id": rows[len(rows)-1].ID})
	}
	resp := gen.ListStaffInvitations200JSONResponse{Items: make([]gen.StaffInvitation, len(rows)), NextCursor: nullableString(next)}
	for i, r := range rows {
		resp.Items[i] = invitationRow(r).api(now)
	}
	return resp, nil
}

// GetStaffInvitation 返回邀请详情。
func (s *Server) GetStaffInvitation(ctx context.Context, req gen.GetStaffInvitationRequestObject) (gen.GetStaffInvitationResponseObject, error) {
	r, err := sqlc.New(s.d.Pool).GetInvitation(ctx, req.Id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.NotFound
	}
	if err != nil {
		return nil, err
	}
	return gen.GetStaffInvitation200JSONResponse(invitationRow(r).api(s.d.Clock.Now())), nil
}

// CreateStaffInvitation 邀请管理员（敏感操作，AUTH-19、AUTH-22）：
//   - 被邀请的邮箱已是管理员，或已有 pending 邀请，返回 400 taken；
//   - 令牌为 32 字节随机值，只存 SHA-256，72 小时有效；响应不返回令牌与链接，只通过邮件送达；
//   - 邀请邮件以 staff_invitation_id 指定收件人（outbox 不保存邮箱，CONV-29），链接加密保存（CONV-31），
//     最长重试时间等于邀请的有效期，语言取邀请人的 locale。
func (s *Server) CreateStaffInvitation(ctx context.Context, req gen.CreateStaffInvitationRequestObject) (gen.CreateStaffInvitationResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	reason, err := bodyReason(req.Body.Reason)
	if err != nil {
		return nil, err
	}
	email, err := account.NormalizeEmail(string(req.Body.Email))
	if err != nil {
		return nil, err
	}
	roles, err := checkRoles(ctx, sqlc.New(s.d.Pool), req.Body.Roles)
	if err != nil {
		return nil, err
	}
	if s.d.AdminURL == "" {
		return nil, errors.New("consoleapi: admin public URL is not configured")
	}
	if err := s.requireStepUp(ctx); err != nil {
		return nil, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	p, _ := auth.FromContext(ctx)
	now := s.d.Clock.Now()
	var out gen.StaffInvitation
	err = pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		// 同一邮箱的并发邀请串行化，使 taken 检查与插入之间不会并发。
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('staff_invitation:' || $1))`, email); err != nil {
			return err
		}
		staffTaken, err := q.StaffEmailExists(ctx, email)
		if err != nil {
			return err
		}
		pending, err := q.PendingInvitationExists(ctx, sqlc.PendingInvitationExistsParams{Email: email, Now: now})
		if err != nil {
			return err
		}
		if staffTaken || pending {
			return apierr.Invalid(apierr.Field("email", "taken"))
		}
		expires := now.Add(InvitationTTL)
		row, err := q.InsertInvitation(ctx, sqlc.InsertInvitationParams{
			Email: email, TokenHash: invitationTokenHash(token), InviterID: p.AccountID, ExpiresAt: expires,
		})
		if err != nil {
			return err
		}
		for _, r := range roles {
			if err := q.InsertInvitationRole(ctx, sqlc.InsertInvitationRoleParams{StaffInvitationID: row.ID, Role: r}); err != nil {
				return err
			}
		}
		inviter, err := q.AccountByID(ctx, p.AccountID)
		if err != nil {
			return err
		}
		id := row.ID
		if err := s.d.Outbox.Enqueue(ctx, q, notify.Message{
			StaffInvitationID: &id, Template: notify.TemplateStaffInvitation, Locale: inviter.Locale,
			Vars:     map[string]string{"hours": strconv.Itoa(int(InvitationTTL / time.Hour))},
			Secrets:  map[string]string{"link": s.d.AdminURL + InvitationPath + "#token=" + token},
			RetryFor: InvitationTTL,
		}); err != nil {
			return err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Action: "staff_invitation.create", TargetType: "staff_invitation", TargetID: row.ID.String(),
			Diff: audit.Values(map[string]any{"roles": roles}), Reason: reason,
		}); err != nil {
			return err
		}
		out = invitationRow{ID: row.ID, Email: email, InviterID: p.AccountID, ExpiresAt: expires, CreatedAt: row.CreatedAt, Roles: roles}.api(now)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateStaffInvitation201JSONResponse(out), nil
}

// RevokeStaffInvitation 撤销 pending 的邀请（敏感操作，AUTH-19）；其他状态返回 409 invalid_state。
// 尚未投递的邀请邮件随之不再投递（AUTH-22）。
func (s *Server) RevokeStaffInvitation(ctx context.Context, req gen.RevokeStaffInvitationRequestObject) (gen.RevokeStaffInvitationResponseObject, error) {
	reason, err := headerReason(req.Params.AuditReason)
	if err != nil {
		return nil, err
	}
	if err := s.requireStepUp(ctx); err != nil {
		return nil, err
	}
	err = pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		inv, err := q.LockInvitation(ctx, req.Id)
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.NotFound
		}
		if err != nil {
			return err
		}
		now := s.d.Clock.Now()
		if inv.AcceptedAt != nil || inv.RevokedAt != nil || !now.Before(inv.ExpiresAt) {
			return apierr.InvalidState
		}
		if err := q.RevokeInvitation(ctx, sqlc.RevokeInvitationParams{ID: inv.ID, Now: &now}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Action: "staff_invitation.revoke", TargetType: "staff_invitation", TargetID: inv.ID.String(), Reason: reason,
		})
	})
	if err != nil {
		return nil, err
	}
	return gen.RevokeStaffInvitation204Response{}, nil
}

// AcceptStaffInvitation 接受管理员邀请（AUTH-22）。邀请行加锁，令牌只能使用一次；按被邀请邮箱的账号状态处理：
//  1. 没有账号：password 必填；注册策略与邮箱域名名单不适用；创建邮箱已验证的账号，生成共用代理凭据并写 credential.changed（AUTH-13）；
//  2. 账号存在且邮箱已验证：忽略 password；
//  3. 账号存在但邮箱未验证：password 必填；替换密码，删除 TOTP 与 Passkey（包括待确认的绑定），
//     吊销全部会话、设备与代理凭据，轮换共用凭据，吊销导出令牌，作废未使用的验证码与找回密码令牌，
//     标记邮箱已验证（防止他人抢先用该邮箱注册后取得管理员身份或保留访问途径）；
//  4. 账号不在正常状态或已经是管理员：409 invalid_state。
//
// 授予邀请中的全部角色，发送 staff_roles_changed，写审计 staff.create。
func (s *Server) AcceptStaffInvitation(ctx context.Context, req gen.AcceptStaffInvitationRequestObject) (gen.AcceptStaffInvitationResponseObject, error) {
	if req.Body == nil || req.Body.Token == "" || len(req.Body.Token) > 128 {
		return nil, apierr.Invalid(apierr.Field("token", "invalid_code"))
	}
	pw := ""
	if req.Body.Password != nil {
		pw = *req.Body.Password
	}
	var (
		out              gen.Staff
		revoked          []uuid.UUID
		credentialsReset bool
		cancelEnroll     uuid.UUID
		passwordHasher   = func() (string, error) {
			if pw == "" {
				return "", apierr.Invalid(apierr.Field("password", "required"))
			}
			if err := password.CheckLength(pw); err != nil {
				code := "too_long"
				if len([]rune(pw)) < password.MinLength {
					code = "too_short"
				}
				return "", apierr.Invalid(apierr.Field("password", code))
			}
			return password.Hash(pw, s.d.Password)
		}
	)
	err := pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		inv, err := q.LockInvitationByToken(ctx, invitationTokenHash(req.Body.Token))
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.Invalid(apierr.Field("token", "invalid_code"))
		}
		if err != nil {
			return err
		}
		now := s.d.Clock.Now()
		switch {
		case inv.AcceptedAt != nil || inv.RevokedAt != nil:
			return apierr.Invalid(apierr.Field("token", "invalid_code"))
		case !now.Before(inv.ExpiresAt):
			return apierr.Invalid(apierr.Field("token", "expired"))
		}
		roles, err := q.InvitationRoles(ctx, inv.ID)
		if err != nil {
			return err
		}
		if len(roles) == 0 {
			return apierr.InvalidState // 邀请的角色已全部被删除
		}
		acct, err := q.LockAccountByEmail(ctx, inv.Email)
		isNew := errors.Is(err, pgx.ErrNoRows)
		var id uuid.UUID
		switch {
		case isNew:
			hash, err := passwordHasher()
			if err != nil {
				return err
			}
			code, err := account.UniqueReferralCode(ctx, q)
			if err != nil {
				return err
			}
			if id, err = q.CreateAccount(ctx, sqlc.CreateAccountParams{
				Email: inv.Email, PasswordHash: &hash, EmailVerifiedAt: &now, ReferralCode: code,
			}); err != nil {
				return err
			}
			if _, err := account.CreateSharedCredential(ctx, q, s.d.Keys, id); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			id = acct.ID
			if acct.Status != "active" {
				return apierr.InvalidState
			}
			existing, err := q.AccountRoles(ctx, id)
			if err != nil {
				return err
			}
			if len(existing) > 0 {
				return apierr.InvalidState
			}
			if acct.EmailVerifiedAt == nil {
				hash, err := passwordHasher()
				if err != nil {
					return err
				}
				if err := q.SetPasswordHash(ctx, sqlc.SetPasswordHashParams{ID: id, PasswordHash: &hash}); err != nil {
					return err
				}
				if err := q.DeleteTotp(ctx, id); err != nil {
					return err
				}
				if err := q.DeleteWebauthn(ctx, id); err != nil {
					return err
				}
				if revoked, err = q.RevokeAccountSessions(ctx, sqlc.RevokeAccountSessionsParams{AccountID: id, Now: &now}); err != nil {
					return err
				}
				if err := resetCredentials(ctx, q, s.d.Keys, id, now); err != nil {
					return err
				}
				if err := q.MarkEmailVerified(ctx, sqlc.MarkEmailVerifiedParams{ID: id, Now: &now}); err != nil {
					return err
				}
				credentialsReset = true
				cancelEnroll = id
			}
		}
		if err := lockStaff(ctx, tx); err != nil {
			return err
		}
		for _, r := range roles {
			if err := q.GrantRole(ctx, sqlc.GrantRoleParams{AccountID: id, Role: r}); err != nil {
				return err
			}
		}
		if err := q.AcceptInvitation(ctx, sqlc.AcceptInvitationParams{ID: inv.ID, AccountID: &id, Now: &now}); err != nil {
			return err
		}
		if err := s.notifyRolesChanged(ctx, q, id, roles); err != nil {
			return err
		}
		diff := map[string]any{"roles": roles, "staff_invitation_id": inv.ID, "inviter_id": inv.InviterID, "is_new_account": isNew}
		if credentialsReset {
			diff["has_credentials_reset"] = true
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Action: "staff.create", TargetType: "account", TargetID: id.String(), Actor: &id,
			Diff: audit.Values(diff),
		}); err != nil {
			return err
		}
		out, err = s.getStaff(ctx, q, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	if cancelEnroll != uuid.Nil {
		// 待确认的 TOTP 绑定保存在 Valkey 中；失败时最多残留 10 分钟，且确认必须来自已被吊销的会话链（AUTH-11）。
		_ = s.d.Sessions.MFA.CancelEnrollment(ctx, cancelEnroll)
	}
	if err := s.d.Sessions.AfterRevoke(ctx, revoked); err != nil {
		return nil, apierr.Unavailable(err)
	}
	return gen.AcceptStaffInvitation200JSONResponse(out), nil
}

// resetCredentials 清除账号在接受邀请前可能被他人持有的全部访问途径（AUTH-22 第 3 项）：
// 吊销设备与全部代理凭据（各写 credential.changed revoked），生成新的共用凭据（rotated），
// 删除导出令牌，作废未使用的验证码与找回密码令牌。
func resetCredentials(ctx context.Context, q *sqlc.Queries, keys *secretbox.Keyring, id uuid.UUID, now time.Time) error {
	if err := q.RevokeAccountDevices(ctx, sqlc.RevokeAccountDevicesParams{AccountID: id, Now: &now}); err != nil {
		return err
	}
	creds, err := q.RevokeAccountCredentials(ctx, sqlc.RevokeAccountCredentialsParams{AccountID: id, Now: &now})
	if err != nil {
		return err
	}
	for _, c := range creds {
		if err := account.CredentialChanged(ctx, q, id, c, "revoked"); err != nil {
			return err
		}
	}
	if _, err := account.RotateSharedCredential(ctx, q, keys, id); err != nil {
		return err
	}
	if err := q.DeleteExportToken(ctx, id); err != nil {
		return err
	}
	return q.InvalidateAllVerificationCodes(ctx, sqlc.InvalidateAllVerificationCodesParams{AccountID: &id, Now: &now})
}
