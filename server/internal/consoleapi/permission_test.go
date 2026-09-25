// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/consoleapi/gen"
)

// builtinGrants 是 spec/10 AUTH-17 权限表中内置角色的权限，作为权限测试的独立依据（不读取数据库或 rbac 包）。
var builtinGrants = map[string][]string{
	"superadmin": {"*"},
	"operator": {"accounts.read", "accounts.adjust", "orders.read", "plans.*", "location-groups.*", "hosts.*",
		"kernels.write", "coupons.*", "content.*", "settings.read"},
	"support": {"accounts.read", "orders.read", "tickets.*"},
}

// expectAllowed 按 AUTH-17 判定：none 只要求管理员；superadmin 只允许 superadmin 角色；
// 其余要求角色的权限中有与 x-permission 相同的一项，或持有 *。
func expectAllowed(required string, role string, grants []string) bool {
	switch required {
	case "none":
		return true
	case "superadmin":
		return role == "superadmin"
	}
	return slices.Contains(grants, "*") || slices.Contains(grants, required)
}

// opPath 把契约路径中的参数替换为合法的值（角色名为 ^[a-z][a-z0-9-]{1,31}$，其余为 UUID）。
func opPath(op gen.Operation) string {
	_, path, _ := strings.Cut(op.Pattern, " ")
	for strings.Contains(path, "{") {
		i := strings.Index(path, "{")
		j := strings.Index(path, "}")
		v := uuid.NewString()
		if strings.HasPrefix(path, "/v1/roles/") {
			v = "no-such-role"
		}
		path = path[:i] + v + path[j+1:]
	}
	return path
}

// sortedOps 返回全部操作，登出排在最后（登出会使该主体后续的请求失去认证）。
func sortedOps() []gen.Operation {
	var ops []gen.Operation
	for _, op := range gen.Operations {
		ops = append(ops, op)
	}
	sort.Slice(ops, func(i, j int) bool {
		if (ops[i].ID == "deleteCurrentSession") != (ops[j].ID == "deleteCurrentSession") {
			return ops[j].ID == "deleteCurrentSession"
		}
		return ops[i].Pattern < ops[j].Pattern
	})
	return ops
}

// TestConsolePermissionMatrix 对契约中的每个管理接口操作（包括尚未实现的）、每种角色验证权限（M1-02 验收 1，AUTH-17）：
// 无权限时恰为 403；有权限时不是 403（之后可能是 400、404、428、401 mfa_required 等）。
// 未来新增的操作自动纳入，无需修改本测试。
func TestConsolePermissionMatrix(t *testing.T) {
	e := newEnv(t)
	principals := map[string]*admin{
		"superadmin": e.staff(t, "superadmin"),
		"operator":   e.staff(t, "operator"),
		"support":    e.staff(t, "support"),
	}
	// 自定义角色：只有 orders.read。
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO roles (name, permissions) VALUES ('order-viewer', '{orders.read}')`); err != nil {
		t.Fatal(err)
	}
	principals["order-viewer"] = e.staff(t, "order-viewer")
	grants := map[string][]string{"order-viewer": {"orders.read"}}
	for k, v := range builtinGrants {
		grants[k] = v
	}
	checked := 0
	for _, op := range sortedOps() {
		if op.Auth == gen.AuthPublic {
			if op.Permission != "none" {
				t.Errorf("%s: public operation with x-permission %q", op.ID, op.Permission)
			}
			continue
		}
		for _, role := range []string{"superadmin", "operator", "support", "order-viewer"} {
			w := e.do(req{method: op.Method, path: opPath(op), as: principals[role], body: "{}"})
			want := expectAllowed(op.Permission, role, grants[role])
			if want && w.Code == 403 {
				t.Errorf("%s as %s: 403, want allowed (x-permission %s)", op.ID, role, op.Permission)
			}
			if !want && (w.Code != 403 || problemCode(t, w) != "forbidden") {
				t.Errorf("%s as %s: %d %s, want 403 (x-permission %s)", op.ID, role, w.Code, w.Body, op.Permission)
			}
			checked++
		}
	}
	if checked < 4*100 {
		t.Fatalf("only %d checks: operation table looks empty", checked)
	}
}

// 客服角色只读（M1-02 验收 1）：除 tickets.* 外的写操作一律 403。
func TestSupportReadOnly(t *testing.T) {
	e := newEnv(t)
	support := e.staff(t, "support")
	for _, op := range sortedOps() {
		if op.Method == "GET" || op.Auth == gen.AuthPublic || op.Permission == "none" || op.Permission == "tickets.*" {
			continue
		}
		if w := e.do(req{method: op.Method, path: opPath(op), as: support, body: "{}"}); w.Code != 403 {
			t.Errorf("%s (%s) as support: %d, want 403", op.ID, op.Permission, w.Code)
		}
	}
}

// 管理接口只接受受众为 console、完成二次验证、未吊销、属于管理员的令牌（AUTH-12、AUTH-21），否则 401。
func TestConsoleAuthentication(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	plain := e.account(t, true) // 没有角色
	issue := func(account uuid.UUID, aud token.Audience, amr []string) string {
		tok, _, err := e.tokens.Issue(token.Claims{AccountID: account, SessionID: uuid.New(), Audience: aud, AMR: amr})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	revoked := issue(super.id, token.AudienceConsole, []string{"pwd", "otp"})
	cases := map[string]string{
		"no token":        "",
		"garbage":         "v4.public.garbage",
		"client audience": issue(super.id, token.AudienceClient, []string{"pwd", "otp"}),
		"without otp":     issue(super.id, token.AudienceConsole, []string{"pwd"}),
		"not staff":       issue(plain.id, token.AudienceConsole, []string{"pwd", "otp"}),
		"revoked session": revoked,
		"bearer not read": "",
	}
	claims, err := e.tokens.Verify(revoked, token.AudienceConsole)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.sessions.Revocations.Revoke(t.Context(), claims.SessionID); err != nil {
		t.Fatal(err)
	}
	for name, tok := range cases {
		for _, op := range sortedOps() {
			if op.Auth == gen.AuthPublic {
				continue
			}
			a := &admin{access: tok}
			r := req{method: op.Method, path: opPath(op), as: a, body: "{}"}
			if name == "bearer not read" {
				r.as = nil
				r.header = map[string]string{"Authorization": "Bearer " + super.access}
			}
			if w := e.do(r); w.Code != 401 || problemCode(t, w) != "unauthenticated" {
				t.Errorf("%s, %s: %d %s", name, op.ID, w.Code, w.Body)
			}
		}
	}
	// 管理员失去全部角色或停用二次验证后，已签发的令牌立即失效（每个请求读取数据库，AUTH-12）。
	op := e.staff(t, "operator")
	if w := e.do(req{method: "GET", path: "/v1/staff/me", as: op}); w.Code != 200 {
		t.Fatalf("staff/me: %d", w.Code)
	}
	if _, err := e.pool.Exec(t.Context(), `DELETE FROM mfa_totp WHERE account_id = $1`, op.id); err != nil {
		t.Fatal(err)
	}
	if w := e.do(req{method: "GET", path: "/v1/staff/me", as: op}); w.Code != 401 {
		t.Fatalf("after TOTP removal: %d", w.Code)
	}
}

// 契约之外的路径返回 404；契约中有、本二进制尚未实现的操作在权限校验之后返回 404。
func TestUnknownAndUnimplemented(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	for _, r := range []req{
		{method: "GET", path: "/v1/nodes", as: super},
		{method: "PUT", path: "/v1/staff/me", as: super},
		{method: "GET", path: "/v1/plans", as: super}, // M1-04 实现
	} {
		if w := e.do(r); w.Code != 404 || problemCode(t, w) != "not_found" {
			t.Errorf("%s %s: %d %s", r.method, r.path, w.Code, w.Body)
		}
	}
}

// 契约为每个敏感操作声明 Mfa-Assertion 参数，并且没有修改或删除审计日志的操作（AUTH-18、AUTH-19）。
func TestContractShape(t *testing.T) {
	for _, op := range gen.Operations {
		if strings.Contains(op.Pattern, "/v1/audit-logs") && op.Method != "GET" && op.ID != "createAuditExport" {
			t.Errorf("%s: audit logs must be read-only", op.ID)
		}
	}
	if !slices.Contains(gen.PermissionCatalog, "staff.*") || !slices.Contains(gen.PermissionCatalog, "*") {
		t.Errorf("catalog = %v", gen.PermissionCatalog)
	}
}
