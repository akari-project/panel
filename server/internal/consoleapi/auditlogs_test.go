// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"net/url"
	"testing"
	"time"
)

type auditPage struct {
	Items []struct {
		ID         string         `json:"id"`
		ActorID    *string        `json:"actor_id"`
		ActorEmail *string        `json:"actor_email"`
		Action     string         `json:"action"`
		TargetType string         `json:"target_type"`
		TargetID   *string        `json:"target_id"`
		Diff       map[string]any `json:"diff"`
		Reason     *string        `json:"reason"`
		IPPrefix   *string        `json:"ip_prefix"`
		RequestID  string         `json:"request_id"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// 审计日志查询（AUTH-18）：倒序游标分页、按条件筛选；来源只记录 /24 前缀，原因来自 reason_texts。
func TestListAuditLogs(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	e.stepUp(t, super)
	for _, name := range []string{"r-a", "r-b", "r-c"} {
		e.createRole(t, super, name, "orders.read")
	}
	var page auditPage
	w := e.do(req{method: "GET", path: "/v1/audit-logs?limit=2", as: super})
	decode(t, w, &page)
	if w.Code != 200 || len(page.Items) != 2 || page.NextCursor == nil || page.Items[0].Action != "role.create" {
		t.Fatalf("page 1: %d %s", w.Code, w.Body)
	}
	first := page.Items[0]
	if first.ActorEmail == nil || *first.ActorEmail != super.email || first.IPPrefix == nil || *first.IPPrefix != prefix24(e.ip) ||
		first.Reason == nil || *first.Reason != "x" || first.RequestID == "" || first.Diff["permissions"] == nil {
		t.Fatalf("entry = %+v", first)
	}
	seen := map[string]bool{}
	for _, it := range page.Items {
		seen[it.ID] = true
	}
	w = e.do(req{method: "GET", path: "/v1/audit-logs?limit=2&cursor=" + *page.NextCursor, as: super})
	decode(t, w, &page)
	if len(page.Items) != 2 || page.NextCursor == nil { // session.create、step_up.create 与 3 条 role.create，共 5 条
		t.Fatalf("page 2: %s", w.Body)
	}
	for _, it := range page.Items {
		if seen[it.ID] {
			t.Fatalf("entry %s repeated across pages", it.ID)
		}
		seen[it.ID] = true
	}
	w = e.do(req{method: "GET", path: "/v1/audit-logs?limit=2&cursor=" + *page.NextCursor, as: super})
	decode(t, w, &page)
	if len(page.Items) != 1 || page.NextCursor != nil || page.Items[0].Action != "session.create" || seen[page.Items[0].ID] {
		t.Fatalf("page 3: %s", w.Body)
	}
	w = e.do(req{method: "GET", path: "/v1/audit-logs?action=role.create&target_type=role&target_id=r-b&actor_id=" + super.id.String(), as: super})
	decode(t, w, &page)
	if len(page.Items) != 1 || *page.Items[0].TargetID != "r-b" {
		t.Fatalf("filtered: %s", w.Body)
	}
	future := url.QueryEscape(e.clk.Now().Add(time.Hour).Format(time.RFC3339))
	w = e.do(req{method: "GET", path: "/v1/audit-logs?created_from=" + future, as: super})
	decode(t, w, &page)
	if len(page.Items) != 0 {
		t.Fatalf("created_from in the future: %s", w.Body)
	}
	w = e.do(req{method: "GET", path: "/v1/audit-logs/" + first.ID, as: super})
	if w.Code != 200 {
		t.Fatalf("get: %d", w.Code)
	}
	if w := e.do(req{method: "GET", path: "/v1/audit-logs?cursor=eyJ4IjoxfQ", as: super}); w.Code != 400 {
		t.Fatalf("invalid cursor: %d", w.Code)
	}
}

func prefix24(ip string) string {
	u, _ := url.Parse("http://" + ip)
	h := u.Hostname()
	i := len(h) - 1
	for h[i] != '.' {
		i--
	}
	return h[:i] + ".0/24"
}

// 接受邀请接受 Idempotency-Key（未认证请求按“路由 + 键”，CONV-12）：同键同体返回相同结果。
func TestAcceptInvitationIdempotent(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	e.stepUp(t, super)
	_, token := e.invite(t, super, "idem@example.com", "support")
	key := "0192f000-0000-7000-8000-000000000001"
	body := map[string]any{"token": token, "password": "correct horse battery"}
	w1 := e.do(req{method: "POST", path: "/v1/staff-invitations/acceptance", body: body, header: map[string]string{"Idempotency-Key": key}})
	w2 := e.do(req{method: "POST", path: "/v1/staff-invitations/acceptance", body: body, header: map[string]string{"Idempotency-Key": key}})
	if w1.Code != 200 || w2.Code != 200 || w1.Body.String() != w2.Body.String() {
		t.Fatalf("%d %s / %d %s", w1.Code, w1.Body, w2.Code, w2.Body)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_logs WHERE action = 'staff.create'`); n != 1 {
		t.Fatalf("%d staff.create entries", n)
	}
}
