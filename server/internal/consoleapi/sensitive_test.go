// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akari-project/panel/server/internal/consoleapi/gen"
)

// sensitiveFixture 为已实现的敏感操作准备一个参数合法的请求（除 Mfa-Assertion 外）。
type sensitiveFixture func(t *testing.T, e *env, super *admin) req

var sensitiveFixtures = map[string]sensitiveFixture{
	"updateStaff": func(t *testing.T, e *env, super *admin) req {
		other := e.account(t, true, "support")
		return req{method: "PATCH", path: "/v1/staff/" + other.id.String(), body: map[string]any{"roles": []string{"operator"}, "reason": "调岗"}}
	},
	"removeStaff": func(t *testing.T, e *env, super *admin) req {
		other := e.account(t, true, "support")
		return req{method: "DELETE", path: "/v1/staff/" + other.id.String(), header: map[string]string{"Audit-Reason": "%E7%A6%BB%E8%81%8C"}}
	},
	"createStaffInvitation": func(t *testing.T, e *env, super *admin) req {
		return req{method: "POST", path: "/v1/staff-invitations", body: map[string]any{"email": "new@example.com", "roles": []string{"operator"}, "reason": "新同事"}}
	},
	"revokeStaffInvitation": func(t *testing.T, e *env, super *admin) req {
		id := e.invitation(t, super, "pending@example.com", t0.Add(time.Hour), "operator")
		return req{method: "DELETE", path: "/v1/staff-invitations/" + id.String(), header: map[string]string{"Audit-Reason": "x"}}
	},
	"createRole": func(t *testing.T, e *env, super *admin) req {
		return req{method: "POST", path: "/v1/roles", body: map[string]any{"name": "viewer", "permissions": []string{"orders.read"}, "reason": "x"}}
	},
	"updateRole": func(t *testing.T, e *env, super *admin) req {
		etag := e.role(t, "viewer", "orders.read")
		return req{method: "PATCH", path: "/v1/roles/viewer", body: map[string]any{"permissions": []string{"accounts.read"}, "reason": "x"},
			header: map[string]string{"If-Match": etag}}
	},
	"deleteRole": func(t *testing.T, e *env, super *admin) req {
		etag := e.role(t, "viewer", "orders.read")
		return req{method: "DELETE", path: "/v1/roles/viewer", header: map[string]string{"If-Match": etag, "Audit-Reason": "x"}}
	},
}

// isImplemented 报告操作是否已在本二进制中实现。
func (e *env) isImplemented(op gen.Operation) bool {
	_, path, _ := strings.Cut(op.Pattern, " ")
	r := httptest.NewRequest(op.Method, strings.NewReplacer("{id}", "x", "{group_id}", "x").Replace(path), nil)
	_, pattern := e.h.(*router).mux.Handler(r)
	return pattern == op.Pattern
}

// 敏感操作（AUTH-19）：参数合法时，缺少、无效或过期的 Mfa-Assertion 返回 401 mfa_required，
// methods 为 ["totp"]、没有 challenge_id；返回 401 之前不写库（CONV-12，M1-02 验收 3）。
// 已实现的每个敏感操作都必须有 fixture，新增的敏感操作因此不会漏测。
func TestSensitiveOpsRequireAssertion(t *testing.T) {
	for _, op := range gen.Operations {
		if !op.Sensitive {
			continue
		}
		t.Run(op.ID, func(t *testing.T) {
			e := newEnv(t)
			if !e.isImplemented(op) {
				t.Skip("not implemented yet")
			}
			fx, ok := sensitiveFixtures[op.ID]
			if !ok {
				t.Fatalf("sensitive operation %s has no fixture", op.ID)
			}
			super := e.staff(t, "superadmin")
			r := fx(t, e, super)
			r.as, r.sensitive = super, true
			snapshot := e.writeSnapshot(t)
			for name, assertion := range map[string]string{"missing": "", "unknown": "not-an-assertion"} {
				super.assertion = assertion
				w := e.do(r)
				if w.Code != 401 || problemCode(t, w) != "mfa_required" {
					t.Fatalf("%s: %d %s", name, w.Code, w.Body)
				}
				p := problem(t, w)
				if _, has := p["challenge_id"]; has {
					t.Errorf("%s: challenge_id present", name)
				}
				if m, _ := p["methods"].([]any); len(m) != 1 || m[0] != "totp" {
					t.Errorf("%s: methods = %v", name, p["methods"])
				}
			}
			// 过期：5 分钟后不再有效。
			e.stepUp(t, super)
			e.clk.Advance(5*time.Minute + time.Second)
			if w := e.do(r); w.Code != 401 || problemCode(t, w) != "mfa_required" {
				t.Fatalf("expired: %d %s", w.Code, w.Body)
			}
			if got := e.writeSnapshot(t); got != snapshot+1 { // 只多了 step-up 的审计记录
				t.Fatalf("writes before 401: %d -> %d", snapshot, got)
			}
			// 有效的 Mfa-Assertion：执行成功。
			e.stepUp(t, super)
			if w := e.do(r); w.Code >= 300 {
				t.Fatalf("with assertion: %d %s", w.Code, w.Body)
			}
		})
	}
}

// writeSnapshot 汇总会被管理操作写入的表的行数。
func (e *env) writeSnapshot(t *testing.T) int {
	return e.count(t, `SELECT (SELECT count(*) FROM audit_logs) + (SELECT count(*) FROM account_roles) + (SELECT count(*) FROM roles)
		+ (SELECT count(*) FROM staff_invitations) + (SELECT count(*) FROM staff_invitations WHERE revoked_at IS NOT NULL)
		+ (SELECT count(*) FROM notification_outbox) + (SELECT count(*) FROM reason_texts)`)
}

// 校验顺序（AUTH-19）：权限（403）→ 参数与原因（400）→ Mfa-Assertion（401）。
func TestSensitiveCheckOrder(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	op := e.staff(t, "operator")
	body := map[string]any{"name": "viewer", "permissions": []string{"orders.read"}, "reason": ""}
	if w := e.do(req{method: "POST", path: "/v1/roles", as: op, body: body}); w.Code != 403 {
		t.Fatalf("operator: %d", w.Code)
	}
	w := e.do(req{method: "POST", path: "/v1/roles", as: super, body: body})
	if f, c := fieldCode(t, w); w.Code != 400 || f != "reason" || c != "required" {
		t.Fatalf("missing reason: %d %s", w.Code, w.Body)
	}
	for name, v := range map[string]string{"missing": "", "undecodable": "%E7%A6", "too long": strings.Repeat("%E5%91%98", 501)} {
		r := req{method: "DELETE", path: "/v1/staff/" + op.id.String(), as: super}
		if v != "" {
			r.header = map[string]string{"Audit-Reason": v}
		}
		w := e.do(r)
		if f, _ := fieldCode(t, w); w.Code != 400 || f != "Audit-Reason" {
			t.Errorf("Audit-Reason %s: %d %s", name, w.Code, w.Body)
		}
	}
	// 500 个码点恰好允许（上限按解码后计算）。
	r := req{method: "DELETE", path: "/v1/staff/" + op.id.String(), as: super, header: map[string]string{"Audit-Reason": strings.Repeat("%E5%91%98", 500)}}
	if w := e.do(r); w.Code != 401 {
		t.Fatalf("500 code points: %d %s", w.Code, w.Body)
	}
}
