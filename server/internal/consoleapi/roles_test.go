// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"fmt"
	"testing"
	"time"
)

func (e *env) createRole(t *testing.T, a *admin, name string, perms ...string) string {
	t.Helper()
	w := e.do(req{method: "POST", path: "/v1/roles", as: a, sensitive: true,
		body: map[string]any{"name": name, "permissions": perms, "description": "d", "reason": "x"}})
	if w.Code != 201 || w.Header().Get("ETag") == "" {
		t.Fatalf("create role %s: %d %s", name, w.Code, w.Body)
	}
	return w.Header().Get("ETag")
}

// 自定义角色（AUTH-22，M1-02 验收 2）：权限必须来自目录，不能包含 * 与 staff.*；名称重复 400 taken。
func TestCreateRole(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	e.stepUp(t, super)
	etag := e.createRole(t, super, "billing", "orders.read", "credits.adjust")
	for name, c := range map[string]struct {
		body  map[string]any
		field string
		code  string
	}{
		"star":      {map[string]any{"name": "r1", "permissions": []string{"*"}, "reason": "x"}, "permissions", "not_allowed"},
		"staff":     {map[string]any{"name": "r2", "permissions": []string{"orders.read", "staff.*"}, "reason": "x"}, "permissions", "not_allowed"},
		"unknown":   {map[string]any{"name": "r3", "permissions": []string{"nodes.write"}, "reason": "x"}, "", ""},
		"bad name":  {map[string]any{"name": "Bad_Name", "permissions": []string{"orders.read"}, "reason": "x"}, "name", "invalid_format"},
		"duplicate": {map[string]any{"name": "billing", "permissions": []string{"orders.read"}, "reason": "x"}, "name", "taken"},
		"builtin":   {map[string]any{"name": "support", "permissions": []string{"orders.read"}, "reason": "x"}, "name", "taken"},
	} {
		w := e.do(req{method: "POST", path: "/v1/roles", as: super, sensitive: true, body: c.body})
		f, code := fieldCode(t, w)
		if w.Code != 400 || (c.field != "" && (f != c.field || code != c.code)) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	w := e.do(req{method: "GET", path: "/v1/roles/billing", as: super})
	if w.Code != 200 || w.Header().Get("ETag") != etag {
		t.Fatalf("get: %d etag %q vs %q", w.Code, w.Header().Get("ETag"), etag)
	}
	if w := e.do(req{method: "GET", path: "/v1/roles/billing", as: super, header: map[string]string{"If-None-Match": etag}}); w.Code != 304 {
		t.Fatalf("If-None-Match: %d", w.Code)
	}
	var list struct {
		Items []struct {
			Name       string `json:"name"`
			IsBuiltin  bool   `json:"is_builtin"`
			StaffCount int    `json:"staff_count"`
		} `json:"items"`
	}
	w = e.do(req{method: "GET", path: "/v1/roles", as: super})
	decode(t, w, &list)
	if len(list.Items) != 4 || list.Items[0].Name != "operator" || !list.Items[0].IsBuiltin || list.Items[3].Name != "billing" {
		t.Fatalf("list = %+v", list.Items)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_logs WHERE action = 'role.create' AND target_id = 'billing'`); n != 1 {
		t.Fatal("role.create not audited")
	}
}

// 角色总数上限 100（含内置角色），超过时创建返回 409 invalid_state。
func TestRoleLimit(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	for i := range MaxRoles - 3 {
		e.role(t, fmt.Sprintf("r-%d", i), "orders.read")
	}
	e.stepUp(t, super)
	w := e.do(req{method: "POST", path: "/v1/roles", as: super, sensitive: true,
		body: map[string]any{"name": "one-more", "permissions": []string{"orders.read"}, "reason": "x"}})
	if w.Code != 409 || problemCode(t, w) != "invalid_state" {
		t.Fatalf("101st role: %d %s", w.Code, w.Body)
	}
}

// 修改角色（CONV-28、AUTH-21、AUTH-22）：缺少 If-Match 428，不一致 409 conflict；内置角色 409 invalid_state；
// 权限变化吊销持有者的管理会话。
func TestUpdateRole(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	e.stepUp(t, super)
	etag := e.createRole(t, super, "billing", "orders.read")
	holder := e.staff(t, "billing")
	patch := func(name, ifMatch string, body map[string]any) (int, string) {
		r := req{method: "PATCH", path: "/v1/roles/" + name, as: super, sensitive: true, body: body}
		if ifMatch != "" {
			r.header = map[string]string{"If-Match": ifMatch}
		}
		w := e.do(r)
		return w.Code, problemCodeOrEmpty(w.Body.Bytes())
	}
	body := map[string]any{"permissions": []string{"orders.read", "accounts.read"}, "reason": "x"}
	if c, code := patch("billing", "", body); c != 428 || code != "precondition_required" {
		t.Fatalf("no If-Match: %d %s", c, code)
	}
	if c, code := patch("billing", `"stale"`, body); c != 409 || code != "conflict" {
		t.Fatalf("stale If-Match: %d %s", c, code)
	}
	if c, code := patch("support", `"x"`, body); c != 409 || code != "invalid_state" {
		t.Fatalf("builtin: %d %s", c, code)
	}
	if c, _ := patch("billing", etag, map[string]any{"permissions": []string{"staff.*"}, "reason": "x"}); c != 400 {
		t.Fatalf("staff.*: %d", c)
	}
	e.clk.Advance(time.Second)
	if c, code := patch("billing", etag, body); c != 200 {
		t.Fatalf("update: %d %s", c, code)
	}
	if w := e.do(req{method: "GET", path: "/v1/staff/me", as: holder}); w.Code != 401 {
		t.Fatalf("holder session after permission change: %d", w.Code)
	}
	if c, code := patch("billing", etag, body); c != 409 || code != "conflict" {
		t.Fatalf("reused ETag: %d %s", c, code)
	}
}

// 删除角色（AUTH-22，M1-02 验收 2）：仍有持有者或仍被 pending 邀请引用返回 409 invalid_state；
// 过期邀请的引用不阻止删除，删除后级联清除。
func TestDeleteRole(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	e.stepUp(t, super)
	del := func(name string) (int, string) {
		etag := e.do(req{method: "GET", path: "/v1/roles/" + name, as: super}).Header().Get("ETag")
		w := e.do(req{method: "DELETE", path: "/v1/roles/" + name, as: super, sensitive: true,
			header: map[string]string{"If-Match": etag, "Audit-Reason": "x"}})
		return w.Code, problemCodeOrEmpty(w.Body.Bytes())
	}
	if c, code := del("operator"); c != 409 || code != "invalid_state" {
		t.Fatalf("builtin: %d %s", c, code)
	}
	e.createRole(t, super, "held", "orders.read")
	e.account(t, true, "held")
	if c, _ := del("held"); c != 409 {
		t.Fatalf("held: %d", c)
	}
	e.createRole(t, super, "invited", "orders.read")
	inv := e.invitation(t, super, "i@example.com", e.clk.Now().Add(time.Minute), "invited")
	if c, _ := del("invited"); c != 409 {
		t.Fatalf("pending invitation: %d", c)
	}
	e.clk.Advance(2 * time.Minute) // 邀请过期
	if c, code := del("invited"); c != 204 {
		t.Fatalf("after invitation expired: %d %s", c, code)
	}
	if n := e.count(t, `SELECT count(*) FROM staff_invitation_roles WHERE staff_invitation_id = $1`, inv); n != 0 {
		t.Fatal("staff_invitation_roles not cascaded")
	}
	if n := e.count(t, `SELECT count(*) FROM audit_logs WHERE action = 'role.delete' AND target_id = 'invited'`); n != 1 {
		t.Fatal("role.delete not audited")
	}
}

func problemCodeOrEmpty(b []byte) string {
	var p struct {
		Code string `json:"code"`
	}
	_ = jsonUnmarshal(b, &p)
	return p.Code
}
