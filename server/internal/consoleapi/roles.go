// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oapi-codegen/nullable"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/audit"
	"github.com/akari-project/panel/server/internal/consoleapi/gen"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/rbac"
)

// MaxRoles 是角色总数上限（含 3 个内置角色，契约 listRoles）。
const MaxRoles = 100

var roleName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// roleETag 是角色的强 ETag（CONV-13），由 updated_at 生成（CONV-28）。
func roleETag(updated time.Time) string {
	return `"` + strconv.FormatInt(updated.UnixMicro(), 36) + `"`
}

type roleRow struct {
	Name        string
	Description *string
	Permissions []string
	IsBuiltin   bool
	StaffCount  int32
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (r roleRow) api() gen.Role {
	perms := make([]gen.Permission, len(r.Permissions))
	for i, p := range r.Permissions {
		perms[i] = gen.Permission(p)
	}
	d := nullable.NewNullNullable[string]()
	if r.Description != nil {
		d = nullable.NewNullableWithValue(*r.Description)
	}
	return gen.Role{Name: r.Name, Description: d, Permissions: perms, IsBuiltin: r.IsBuiltin, StaffCount: r.StaffCount,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func permissionStrings(ps []gen.Permission) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return out
}

func description(d nullable.Nullable[string]) *string {
	if !d.IsSpecified() || d.IsNull() {
		return nil
	}
	v := d.MustGet()
	return &v
}

// ListRoles 列出全部角色（不分页，总数不超过 100）。
func (s *Server) ListRoles(ctx context.Context, _ gen.ListRolesRequestObject) (gen.ListRolesResponseObject, error) {
	rows, err := sqlc.New(s.d.Pool).ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	resp := gen.ListRoles200JSONResponse{Items: make([]gen.Role, len(rows))}
	for i, r := range rows {
		resp.Items[i] = roleRow(r).api()
	}
	return resp, nil
}

func getRole(ctx context.Context, q *sqlc.Queries, name string) (roleRow, error) {
	r, err := q.GetRole(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return roleRow{}, apierr.NotFound
	}
	if err != nil {
		return roleRow{}, err
	}
	return roleRow(r), nil
}

// GetRole 返回角色详情，带强 ETag，支持 If-None-Match（CONV-13）。
func (s *Server) GetRole(ctx context.Context, req gen.GetRoleRequestObject) (gen.GetRoleResponseObject, error) {
	r, err := getRole(ctx, sqlc.New(s.d.Pool), req.Id)
	if err != nil {
		return nil, err
	}
	etag := roleETag(r.UpdatedAt)
	if req.Params.IfNoneMatch != nil && *req.Params.IfNoneMatch == etag {
		return gen.GetRole304Response{Headers: gen.NotModifiedResponseHeaders{ETag: &etag}}, nil
	}
	return gen.GetRole200JSONResponse{Body: r.api(), Headers: gen.GetRole200ResponseHeaders{ETag: &etag}}, nil
}

// CreateRole 创建自定义角色（敏感操作，AUTH-19、AUTH-22）：权限必须来自权限目录，不能包含 * 与 staff.*；
// 名称已被占用返回 400 taken；角色总数达到 100 返回 409 invalid_state。
func (s *Server) CreateRole(ctx context.Context, req gen.CreateRoleRequestObject) (gen.CreateRoleResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	reason, err := bodyReason(req.Body.Reason)
	if err != nil {
		return nil, err
	}
	if !roleName.MatchString(req.Body.Name) {
		return nil, apierr.Invalid(apierr.Field("name", "invalid_format"))
	}
	perms := permissionStrings(req.Body.Permissions)
	if err := rbac.CheckCustomPermissions(perms, gen.PermissionCatalog); err != nil {
		return nil, err
	}
	desc := description(req.Body.Description)
	if err := s.requireStepUp(ctx); err != nil {
		return nil, err
	}
	var out roleRow
	err = pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		// 串行化角色的创建，使总数检查与插入之间不会并发超额。
		if _, err := tx.Exec(ctx, `LOCK TABLE roles IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		n, err := q.CountRoles(ctx)
		if err != nil {
			return err
		}
		if n >= MaxRoles {
			return apierr.InvalidState
		}
		if err := q.InsertRole(ctx, sqlc.InsertRoleParams{Name: req.Body.Name, Description: desc, Permissions: perms}); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == "23505" {
				return apierr.Invalid(apierr.Field("name", "taken"))
			}
			return err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Action: "role.create", TargetType: "role", TargetID: req.Body.Name,
			Diff: audit.Values(map[string]any{"description": desc, "permissions": perms}), Reason: reason,
		}); err != nil {
			return err
		}
		out, err = getRole(ctx, q, req.Body.Name)
		return err
	})
	if err != nil {
		return nil, err
	}
	etag := roleETag(out.UpdatedAt)
	return gen.CreateRole201JSONResponse{Body: out.api(), Headers: gen.CreateRole201ResponseHeaders{ETag: &etag}}, nil
}

// lockCustomRole 锁定要修改或删除的角色，并检查 If-Match（CONV-28）：不存在返回 404，内置角色返回 409 invalid_state，
// ETag 不一致返回 409 conflict。
func lockCustomRole(ctx context.Context, q *sqlc.Queries, name, ifMatch string) (sqlc.LockRoleRow, error) {
	r, err := q.LockRole(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, apierr.NotFound
	}
	if err != nil {
		return r, err
	}
	if r.IsBuiltin {
		return r, apierr.InvalidState
	}
	if ifMatch != roleETag(r.UpdatedAt) {
		return r, apierr.Conflict
	}
	return r, nil
}

// revokeHolders 吊销持有该角色的管理员的全部管理会话（AUTH-21）。
func (s *Server) revokeHolders(ctx context.Context, q *sqlc.Queries, role string) ([]uuid.UUID, error) {
	holders, err := q.RoleHolders(ctx, role)
	if err != nil {
		return nil, err
	}
	now := s.d.Clock.Now()
	var all []uuid.UUID
	for _, h := range holders {
		ids, err := q.RevokeConsoleSessions(ctx, sqlc.RevokeConsoleSessionsParams{AccountID: h, Now: &now})
		if err != nil {
			return nil, err
		}
		all = append(all, ids...)
	}
	return all, nil
}

// UpdateRole 修改自定义角色（敏感操作，AUTH-19、AUTH-22）；内置角色返回 409 invalid_state。
// 必须携带 If-Match（CONV-28）。变更后吊销持有该角色的管理员的全部管理会话（AUTH-21）。
func (s *Server) UpdateRole(ctx context.Context, req gen.UpdateRoleRequestObject) (gen.UpdateRoleResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	reason, err := bodyReason(req.Body.Reason)
	if err != nil {
		return nil, err
	}
	var perms []string
	if req.Body.Permissions != nil {
		perms = permissionStrings(*req.Body.Permissions)
		if err := rbac.CheckCustomPermissions(perms, gen.PermissionCatalog); err != nil {
			return nil, err
		}
	}
	if err := s.requireStepUp(ctx); err != nil {
		return nil, err
	}
	var (
		out     roleRow
		revoked []uuid.UUID
	)
	err = pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		ifMatch := ""
		if req.Params.IfMatch != nil {
			ifMatch = *req.Params.IfMatch
		}
		cur, err := lockCustomRole(ctx, q, req.Id, ifMatch)
		if err != nil {
			return err
		}
		desc := cur.Description
		if req.Body.Description.IsSpecified() {
			desc = description(req.Body.Description)
		}
		if perms == nil {
			perms = cur.Permissions
		}
		before := map[string]any{"description": cur.Description, "permissions": cur.Permissions}
		after := map[string]any{"description": desc, "permissions": perms}
		changes := audit.Changes(before, after)
		if len(changes) > 0 {
			if err := q.UpdateRole(ctx, sqlc.UpdateRoleParams{Name: req.Id, Description: desc, Permissions: perms}); err != nil {
				return err
			}
			if !slices.Equal(cur.Permissions, perms) {
				if revoked, err = s.revokeHolders(ctx, q, req.Id); err != nil {
					return err
				}
			}
			if err := audit.Record(ctx, q, audit.Entry{
				Action: "role.update", TargetType: "role", TargetID: req.Id, Diff: changes, Reason: reason,
			}); err != nil {
				return err
			}
		}
		out, err = getRole(ctx, q, req.Id)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := s.d.Sessions.AfterRevoke(ctx, revoked); err != nil {
		return nil, apierr.Unavailable(err)
	}
	etag := roleETag(out.UpdatedAt)
	return gen.UpdateRole200JSONResponse{Body: out.api(), Headers: gen.UpdateRole200ResponseHeaders{ETag: &etag}}, nil
}

// DeleteRole 删除自定义角色（敏感操作，AUTH-19、AUTH-22）。内置角色、仍有管理员持有的角色、
// 仍被 pending 邀请引用的角色返回 409 invalid_state。必须携带 If-Match（CONV-28）。
func (s *Server) DeleteRole(ctx context.Context, req gen.DeleteRoleRequestObject) (gen.DeleteRoleResponseObject, error) {
	reason, err := headerReason(req.Params.AuditReason)
	if err != nil {
		return nil, err
	}
	if err := s.requireStepUp(ctx); err != nil {
		return nil, err
	}
	err = pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if err := lockStaff(ctx, tx); err != nil {
			return err
		}
		ifMatch := ""
		if req.Params.IfMatch != nil {
			ifMatch = *req.Params.IfMatch
		}
		cur, err := lockCustomRole(ctx, q, req.Id, ifMatch)
		if err != nil {
			return err
		}
		holders, err := q.CountRoleMembers(ctx, req.Id)
		if err != nil {
			return err
		}
		pending, err := q.RoleHasPendingInvitation(ctx, sqlc.RoleHasPendingInvitationParams{Role: req.Id, Now: s.d.Clock.Now()})
		if err != nil {
			return err
		}
		if holders > 0 || pending {
			return apierr.InvalidState
		}
		if err := q.ClearLegacyInvitationRole(ctx, &req.Id); err != nil {
			return err
		}
		if err := q.DeleteRole(ctx, req.Id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Action: "role.delete", TargetType: "role", TargetID: req.Id,
			Diff: audit.Values(map[string]any{"permissions": cur.Permissions}), Reason: reason,
		})
	})
	if err != nil {
		return nil, err
	}
	return gen.DeleteRole204Response{}, nil
}
