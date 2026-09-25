// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/audit"
	"github.com/akari-project/panel/server/internal/consoleapi/gen"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/notify"
	"github.com/akari-project/panel/server/internal/rbac"
)

// staffRow 是 ListStaff 与 GetStaff 共有的列。
type staffRow struct {
	ID         uuid.UUID
	Email      string
	Roles      []string
	HasTotp    bool
	HasPasskey bool
	CreatedAt  time.Time
}

func (s *Server) staffList(ctx context.Context, q *sqlc.Queries, rows []staffRow) ([]gen.Staff, error) {
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	logins, err := q.StaffLastLogins(ctx, ids)
	if err != nil {
		return nil, err
	}
	last := map[uuid.UUID]time.Time{}
	for _, l := range logins {
		last[l.AccountID] = l.LastLoginAt
	}
	out := make([]gen.Staff, len(rows))
	for i, r := range rows {
		out[i] = gen.Staff{AccountId: r.ID, Email: openapi_types.Email(r.Email), Roles: r.Roles, HasTotp: r.HasTotp,
			HasPasskey: r.HasPasskey, CreatedAt: r.CreatedAt, LastLoginAt: nullable.NewNullNullable[time.Time]()}
		if t, ok := last[r.ID]; ok {
			out[i].LastLoginAt = nullable.NewNullableWithValue(t)
		}
	}
	return out, nil
}

func (s *Server) getStaff(ctx context.Context, q *sqlc.Queries, id uuid.UUID) (gen.Staff, error) {
	r, err := q.GetStaff(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Staff{}, apierr.NotFound
	}
	if err != nil {
		return gen.Staff{}, err
	}
	list, err := s.staffList(ctx, q, []staffRow{staffRow(r)})
	if err != nil {
		return gen.Staff{}, err
	}
	return list[0], nil
}

// ListStaff 列出管理员（持有至少一个角色的账号），按账号 ID 分页（CONV-11）。
func (s *Server) ListStaff(ctx context.Context, req gen.ListStaffRequestObject) (gen.ListStaffResponseObject, error) {
	limit, err := pageLimit(req.Params.Limit)
	if err != nil {
		return nil, err
	}
	var cur struct {
		ID uuid.UUID `json:"id"`
	}
	var after *uuid.UUID
	if ok, err := decodeCursor(req.Params.Cursor, &cur); err != nil {
		return nil, err
	} else if ok {
		after = &cur.ID
	}
	q := sqlc.New(s.d.Pool)
	rows, err := q.ListStaff(ctx, sqlc.ListStaffParams{Role: req.Params.Role, After: after, MaxRows: limit + 1})
	if err != nil {
		return nil, err
	}
	var next *string
	if len(rows) > int(limit) {
		rows = rows[:limit]
		next = encodeCursor(map[string]any{"id": rows[len(rows)-1].ID})
	}
	conv := make([]staffRow, len(rows))
	for i, r := range rows {
		conv[i] = staffRow(r)
	}
	items, err := s.staffList(ctx, q, conv)
	if err != nil {
		return nil, err
	}
	resp := gen.ListStaff200JSONResponse{Items: items}
	resp.NextCursor = nullableString(next)
	return resp, nil
}

// GetStaff 返回管理员详情；不是管理员的账号返回 404。
func (s *Server) GetStaff(ctx context.Context, req gen.GetStaffRequestObject) (gen.GetStaffResponseObject, error) {
	st, err := s.getStaff(ctx, sqlc.New(s.d.Pool), req.Id)
	if err != nil {
		return nil, err
	}
	return gen.GetStaff200JSONResponse(st), nil
}

// checkRoles 校验要授予的角色：非空、不重复、都已存在（AUTH-22）。字段错误的 field 为 roles。
func checkRoles(ctx context.Context, q *sqlc.Queries, roles []string) ([]string, error) {
	if len(roles) == 0 {
		return nil, apierr.Invalid(apierr.Field("roles", "required"))
	}
	out := slices.Clone(roles)
	slices.Sort(out)
	if len(slices.Compact(out)) != len(roles) {
		return nil, apierr.Invalid(apierr.Field("roles", "invalid_format"))
	}
	out = slices.Compact(out)
	n, err := q.CountExistingRoles(ctx, out)
	if err != nil {
		return nil, err
	}
	if int(n) != len(out) {
		return nil, apierr.Invalid(apierr.Field("roles", "invalid_format"))
	}
	return out, nil
}

// lockStaff 串行化管理员角色的变更，使“至少保留一个 superadmin”的检查与修改之间不会并发（AUTH-22）。
func lockStaff(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `LOCK TABLE account_roles IN SHARE ROW EXCLUSIVE MODE`)
	return err
}

// changeRoles 在事务中把 target 的角色改为 roles（为空表示移除管理员），并执行连带处理（AUTH-21、AUTH-22）：
//   - 移除最后一个 superadmin 返回 409 invalid_state；
//   - 吊销 target 的全部管理会话；
//   - target 失去 superadmin 时撤销其发出的全部 pending 邀请，每条写审计 staff_invitation.revoke；
//   - 发送安全通知 staff_roles_changed（OPS-04）。
//
// 返回变更前的角色与被吊销的会话。
func (s *Server) changeRoles(ctx context.Context, tx pgx.Tx, target uuid.UUID, roles []string) ([]string, []uuid.UUID, error) {
	q := sqlc.New(tx)
	if err := lockStaff(ctx, tx); err != nil {
		return nil, nil, err
	}
	before, err := q.AccountRoles(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	if len(before) == 0 {
		return nil, nil, apierr.NotFound
	}
	if slices.Equal(before, roles) {
		return before, nil, errUnchanged
	}
	losesSuper := slices.Contains(before, rbac.SuperadminRole) && !slices.Contains(roles, rbac.SuperadminRole)
	if losesSuper {
		n, err := q.CountRoleMembers(ctx, rbac.SuperadminRole)
		if err != nil {
			return nil, nil, err
		}
		if n <= 1 {
			return nil, nil, apierr.InvalidState
		}
	}
	if err := q.DeleteAccountRoles(ctx, target); err != nil {
		return nil, nil, err
	}
	for _, r := range roles {
		if err := q.GrantRole(ctx, sqlc.GrantRoleParams{AccountID: target, Role: r}); err != nil {
			return nil, nil, err
		}
	}
	now := s.d.Clock.Now()
	revoked, err := q.RevokeConsoleSessions(ctx, sqlc.RevokeConsoleSessionsParams{AccountID: target, Now: &now})
	if err != nil {
		return nil, nil, err
	}
	if losesSuper {
		ids, err := q.RevokePendingInvitationsBy(ctx, sqlc.RevokePendingInvitationsByParams{InviterID: target, Now: &now})
		if err != nil {
			return nil, nil, err
		}
		for _, id := range ids {
			if err := audit.Record(ctx, q, audit.Entry{Action: "staff_invitation.revoke", TargetType: "staff_invitation", TargetID: id.String()}); err != nil {
				return nil, nil, err
			}
		}
	}
	if err := s.notifyRolesChanged(ctx, q, target, roles); err != nil {
		return nil, nil, err
	}
	return before, revoked, nil
}

// errUnchanged：要设置的角色与现有角色相同，没有任何变更（不吊销会话、不通知、不写审计）。
var errUnchanged = errors.New("consoleapi: roles unchanged")

// notifyRolesChanged 发送安全通知 staff_roles_changed（AUTH-22、OPS-04）。
func (s *Server) notifyRolesChanged(ctx context.Context, q *sqlc.Queries, account uuid.UUID, roles []string) error {
	a, err := q.AccountByID(ctx, account)
	if err != nil {
		return err
	}
	return s.d.Outbox.Enqueue(ctx, q, notify.Message{AccountID: account, Template: notify.TemplateStaffRolesChanged, Locale: a.Locale,
		Vars: map[string]string{"roles": strings.Join(roles, ", ")}})
}

// UpdateStaff 修改管理员的角色（敏感操作，AUTH-19、AUTH-22）。变更后吊销该管理员的全部管理会话（AUTH-21）。
func (s *Server) UpdateStaff(ctx context.Context, req gen.UpdateStaffRequestObject) (gen.UpdateStaffResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	reason, err := bodyReason(req.Body.Reason)
	if err != nil {
		return nil, err
	}
	roles, err := checkRoles(ctx, sqlc.New(s.d.Pool), req.Body.Roles)
	if err != nil {
		return nil, err
	}
	if err := s.requireStepUp(ctx); err != nil {
		return nil, err
	}
	var revoked []uuid.UUID
	var out gen.Staff
	err = pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		target := req.Id
		before, rv, err := s.changeRoles(ctx, tx, target, roles)
		if errors.Is(err, errUnchanged) {
			out, err = s.getStaff(ctx, sqlc.New(tx), target)
			return err
		}
		if err != nil {
			return err
		}
		revoked = rv
		if err := audit.Record(ctx, sqlc.New(tx), audit.Entry{
			Action: "staff.update", TargetType: "account", TargetID: target.String(),
			Diff:   audit.Changes(map[string]any{"roles": before}, map[string]any{"roles": roles}),
			Reason: reason, ReasonAccount: &target,
		}); err != nil {
			return err
		}
		out, err = s.getStaff(ctx, sqlc.New(tx), target)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := s.d.Sessions.AfterRevoke(ctx, revoked); err != nil {
		return nil, apierr.Unavailable(err)
	}
	return gen.UpdateStaff200JSONResponse(out), nil
}

// RemoveStaff 移除管理员：删除其全部角色并吊销其管理会话，账号保留（敏感操作，AUTH-19、AUTH-22）。
func (s *Server) RemoveStaff(ctx context.Context, req gen.RemoveStaffRequestObject) (gen.RemoveStaffResponseObject, error) {
	reason, err := headerReason(req.Params.AuditReason)
	if err != nil {
		return nil, err
	}
	if err := s.requireStepUp(ctx); err != nil {
		return nil, err
	}
	var revoked []uuid.UUID
	err = pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		before, rv, err := s.changeRoles(ctx, tx, req.Id, nil)
		if err != nil {
			return err
		}
		revoked = rv
		target := req.Id
		return audit.Record(ctx, sqlc.New(tx), audit.Entry{
			Action: "staff.delete", TargetType: "account", TargetID: target.String(),
			Diff: audit.Values(map[string]any{"roles": before}), Reason: reason, ReasonAccount: &target,
		})
	})
	if err != nil {
		return nil, err
	}
	if err := s.d.Sessions.AfterRevoke(ctx, revoked); err != nil {
		return nil, apierr.Unavailable(err)
	}
	return gen.RemoveStaff204Response{}, nil
}

func nullableString(s *string) nullable.Nullable[string] {
	if s == nil {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(*s)
}
