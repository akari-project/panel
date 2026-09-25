// SPDX-License-Identifier: AGPL-3.0-or-later

// Package rbac 是管理员的角色与权限（spec/10 AUTH-12、AUTH-17、AUTH-22）：
//
//   - 权限为 `资源.动作`，`资源.*` 表示该资源的全部动作，`*` 表示全部权限；
//   - 管理接口每个操作以 x-permission 声明所需权限（契约生成的操作表），另有两个特殊值：
//     none 只要求是管理员，superadmin 只允许 superadmin 角色（不能授予其他角色）；
//   - 管理员即持有至少一个角色、账号正常且启用了二次验证的账号（AUTH-12），每个请求从数据库读取，
//     角色与二次验证的变化立即生效。
package rbac

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// x-permission 的特殊值与内置角色。
const (
	// PermNone：只要求管理员已登录（AUTH-17）。
	PermNone = "none"
	// PermSuperadmin：只允许 superadmin 角色（AUTH-17、AUTH-22）。
	PermSuperadmin = "superadmin"
	// All 是全部权限，只出现在内置角色 superadmin 上。
	All = "*"
	// SuperadminRole 是内置超级管理员角色。
	SuperadminRole = "superadmin"
	// ReservedStaff 是管理员、邀请与角色的权限：保留在目录中，只经 superadmin 的 * 持有（AUTH-22）。
	ReservedStaff = "staff.*"
)

// Staff 是当前请求的管理员。
type Staff struct {
	AccountID   uuid.UUID
	Email       string
	Roles       []string
	Permissions []string
	HasTOTP     bool
	HasPasskey  bool
}

// ErrNotStaff 表示账号不是可以访问管理接口的管理员：没有角色、账号不在正常状态，或未启用二次验证（AUTH-12）。
var ErrNotStaff = errors.New("rbac: not an active staff member with two-factor authentication")

// Load 读取账号的管理员信息。账号不是管理员时返回 ErrNotStaff。
func Load(ctx context.Context, q *sqlc.Queries, account uuid.UUID) (Staff, error) {
	row, err := q.StaffPrincipal(ctx, account)
	if errors.Is(err, pgx.ErrNoRows) {
		return Staff{}, ErrNotStaff
	}
	if err != nil {
		return Staff{}, err
	}
	if row.Status != "active" || len(row.Roles) == 0 || !row.HasTotp {
		return Staff{}, ErrNotStaff
	}
	return Staff{AccountID: account, Email: row.Email, Roles: row.Roles, Permissions: row.Permissions,
		HasTOTP: row.HasTotp, HasPasskey: row.HasPasskey}, nil
}

// IsSuperadmin 报告是否持有 superadmin 角色。
func (s Staff) IsSuperadmin() bool { return slices.Contains(s.Roles, SuperadminRole) }

// Allows 报告管理员是否可以执行 x-permission 为 required 的操作。
func (s Staff) Allows(required string) bool {
	switch required {
	case PermNone:
		return true
	case PermSuperadmin:
		return s.IsSuperadmin()
	case "":
		return false // 契约中每个管理操作都有 x-permission（CI 校验），缺失时拒绝
	}
	for _, g := range s.Permissions {
		if Grants(g, required) {
			return true
		}
	}
	return false
}

// Grants 报告权限 g 是否覆盖 required：相同、g 为 *，或 g 为 `资源.*` 且 required 属于该资源。
func Grants(g, required string) bool {
	if g == All || g == required {
		return true
	}
	if res, ok := strings.CutSuffix(g, ".*"); ok {
		return strings.HasPrefix(required, res+".")
	}
	return false
}

// CheckCustomPermissions 校验自定义角色的权限（AUTH-22）：必须来自权限目录，且不能包含 * 与 staff.*，
// 否则为 not_allowed（生成的类型不校验枚举，目录之外的值在这里拒绝）；重复为 invalid_format。
// 返回的字段错误的 field 为 permissions。
func CheckCustomPermissions(perms, catalog []string) *apierr.Error {
	if len(perms) == 0 {
		return apierr.Invalid(apierr.Field("permissions", "required"))
	}
	seen := map[string]bool{}
	for _, p := range perms {
		switch {
		case !slices.Contains(catalog, p), p == All, p == ReservedStaff:
			return apierr.Invalid(apierr.Field("permissions", "not_allowed"))
		case seen[p]:
			return apierr.Invalid(apierr.Field("permissions", "invalid_format"))
		}
		seen[p] = true
	}
	return nil
}
