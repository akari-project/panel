// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// 修改角色（AUTH-21、AUTH-22）：吊销被修改者的管理会话，发送 staff_roles_changed，审计记录角色前后值与原因。
func TestUpdateStaff(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	target := e.staff(t, "support")
	e.stepUp(t, super)
	w := e.do(req{method: "PATCH", path: "/v1/staff/" + target.id.String(), as: super, sensitive: true,
		body: map[string]any{"roles": []string{"operator", "support"}, "reason": "兼任运营"}})
	if w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	var st struct {
		Roles []string `json:"roles"`
	}
	decode(t, w, &st)
	if !slices.Equal(st.Roles, []string{"operator", "support"}) {
		t.Fatalf("roles = %v", st.Roles)
	}
	if w := e.do(req{method: "GET", path: "/v1/staff/me", as: target}); w.Code != 401 {
		t.Fatalf("target session after role change: %d", w.Code)
	}
	var diff, reason string
	if err := e.pool.QueryRow(t.Context(), `SELECT l.diff::text, r.body FROM audit_logs l JOIN reason_texts r ON r.id = l.reason_id
		WHERE l.action = 'staff.update' AND l.target_id = $1 AND r.account_id = $2`, target.id.String(), target.id).Scan(&diff, &reason); err != nil {
		t.Fatal(err)
	}
	if reason != "兼任运营" || !strings.Contains(diff, `"from": ["support"]`) || !strings.Contains(diff, `"to": ["operator", "support"]`) {
		t.Fatalf("diff %s reason %q", diff, reason)
	}
	if n := e.count(t, `SELECT count(*) FROM notification_outbox WHERE account_id = $1 AND template = 'staff_roles_changed'`, target.id); n != 1 {
		t.Fatalf("%d staff_roles_changed notifications", n)
	}
	// 不存在的角色、空角色：400。
	for _, roles := range [][]string{{"no-such"}, {}} {
		w := e.do(req{method: "PATCH", path: "/v1/staff/" + target.id.String(), as: super, sensitive: true,
			body: map[string]any{"roles": roles, "reason": "x"}})
		if f, _ := fieldCode(t, w); w.Code != 400 || f != "roles" {
			t.Errorf("roles %v: %d %s", roles, w.Code, w.Body)
		}
	}
	// 不是管理员的账号：404。
	plain := e.account(t, true)
	w = e.do(req{method: "PATCH", path: "/v1/staff/" + plain.id.String(), as: super, sensitive: true,
		body: map[string]any{"roles": []string{"support"}, "reason": "x"}})
	if w.Code != 404 {
		t.Fatalf("non-staff: %d %s", w.Code, w.Body)
	}
}

// 至少保留一个 superadmin（AUTH-22）：修改或移除最后一个返回 409 invalid_state；
// 超级管理员被降级时，其 pending 邀请在同一事务中撤销并写审计（M1-02 验收 5）。
func TestLastSuperadmin(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	e.stepUp(t, super)
	w := e.do(req{method: "PATCH", path: "/v1/staff/" + super.id.String(), as: super, sensitive: true,
		body: map[string]any{"roles": []string{"operator"}, "reason": "x"}})
	if w.Code != 409 || problemCode(t, w) != "invalid_state" {
		t.Fatalf("demote last superadmin: %d %s", w.Code, w.Body)
	}
	w = e.do(req{method: "DELETE", path: "/v1/staff/" + super.id.String(), as: super, sensitive: true, header: map[string]string{"Audit-Reason": "x"}})
	if w.Code != 409 {
		t.Fatalf("remove last superadmin: %d %s", w.Code, w.Body)
	}

	second := e.staff(t, "superadmin")
	pending := e.invitation(t, second, "p@example.com", e.clk.Now().Add(time.Hour), "operator")
	expired := e.invitation(t, second, "x@example.com", e.clk.Now().Add(-time.Hour), "operator")
	w = e.do(req{method: "DELETE", path: "/v1/staff/" + second.id.String(), as: super, sensitive: true, header: map[string]string{"Audit-Reason": "%E7%A6%BB%E8%81%8C"}})
	if w.Code != 204 {
		t.Fatalf("remove second superadmin: %d %s", w.Code, w.Body)
	}
	if n := e.count(t, `SELECT count(*) FROM staff_invitations WHERE id = $1 AND revoked_at IS NOT NULL`, pending); n != 1 {
		t.Fatal("pending invitation of the demoted superadmin not revoked")
	}
	if n := e.count(t, `SELECT count(*) FROM staff_invitations WHERE id = $1 AND revoked_at IS NOT NULL`, expired); n != 0 {
		t.Fatal("expired invitation revoked")
	}
	if n := e.count(t, `SELECT count(*) FROM audit_logs WHERE action = 'staff_invitation.revoke' AND target_id = $1`, pending.String()); n != 1 {
		t.Fatal("automatic revocation not audited")
	}
	if n := e.count(t, `SELECT count(*) FROM account_roles WHERE account_id = $1`, second.id); n != 0 {
		t.Fatal("roles not removed")
	}
	if n := e.count(t, `SELECT count(*) FROM audit_logs l JOIN reason_texts r ON r.id = l.reason_id WHERE l.action = 'staff.delete' AND r.body = '离职'`); n != 1 {
		t.Fatal("staff.delete audit with decoded Audit-Reason missing")
	}
}

// 管理员列表与详情：分页、按角色筛选、从未登录的 last_login_at 为 null。
func TestListStaff(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	for range 3 {
		e.account(t, true, "support")
	}
	var page struct {
		Items []struct {
			Roles       []string `json:"roles"`
			LastLoginAt *string  `json:"last_login_at"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	w := e.do(req{method: "GET", path: "/v1/staff?role=support&limit=2", as: super})
	decode(t, w, &page)
	if w.Code != 200 || len(page.Items) != 2 || page.NextCursor == nil || page.Items[0].LastLoginAt != nil {
		t.Fatalf("page 1: %d %s", w.Code, w.Body)
	}
	w = e.do(req{method: "GET", path: "/v1/staff?role=support&limit=2&cursor=" + *page.NextCursor, as: super})
	decode(t, w, &page)
	if len(page.Items) != 1 || page.NextCursor != nil {
		t.Fatalf("page 2: %s", w.Body)
	}
	for _, bad := range []string{"?cursor=!!!", "?cursor=bm9wZQ", "?limit=0", "?limit=201"} {
		if w := e.do(req{method: "GET", path: "/v1/staff" + bad, as: super}); w.Code != 400 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	w = e.do(req{method: "GET", path: "/v1/staff/" + super.id.String(), as: super})
	var one struct {
		LastLoginAt *string `json:"last_login_at"`
	}
	decode(t, w, &one)
	if w.Code != 200 || one.LastLoginAt == nil {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
}
