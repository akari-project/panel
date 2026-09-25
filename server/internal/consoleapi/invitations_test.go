// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/notify"
	"github.com/akari-project/panel/server/internal/password"
)

type captureSender struct {
	mu   sync.Mutex
	sent []notify.Email
}

func (c *captureSender) Send(_ context.Context, m notify.Email) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}

// deliver 投递通知队列，返回发出的邮件。
func (e *env) deliver(t *testing.T) []notify.Email {
	t.Helper()
	s := &captureSender{}
	d := &notify.Deliverer{Pool: e.pool, Keys: e.keys, Clock: e.clk, Sender: s, SiteName: "Akari"}
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s.sent
}

// invite 通过接口邀请 email，投递邀请邮件并返回邀请 ID 与邮件中的令牌。
func (e *env) invite(t *testing.T, super *admin, email string, roles ...string) (uuid.UUID, string) {
	t.Helper()
	w := e.do(req{method: "POST", path: "/v1/staff-invitations", as: super, sensitive: true,
		body: map[string]any{"email": email, "roles": roles, "reason": "新同事"}})
	if w.Code != 201 {
		t.Fatalf("invite: %d %s", w.Code, w.Body)
	}
	var out struct {
		ID     uuid.UUID `json:"id"`
		Status string    `json:"status"`
	}
	decode(t, w, &out)
	if out.Status != "pending" || strings.Contains(w.Body.String(), "token") {
		t.Fatalf("response %s", w.Body)
	}
	for _, m := range e.deliver(t) {
		if m.To == email {
			_, token, ok := strings.Cut(m.Body, "https://console.example.com/accept-invitation#token=")
			if !ok {
				t.Fatalf("invitation link missing in %q", m.Body)
			}
			token, _, _ = strings.Cut(token, "\n")
			return out.ID, token
		}
	}
	t.Fatal("invitation email not sent")
	return uuid.Nil, ""
}

func (e *env) accept(t *testing.T, token string, pw *string) (int, string, string) {
	t.Helper()
	body := map[string]any{"token": token}
	if pw != nil {
		body["password"] = *pw
	}
	w := e.do(req{method: "POST", path: "/v1/staff-invitations/acceptance", body: body})
	f, c := fieldCode(t, w)
	if c == "" {
		c = problemCodeOrEmpty(w.Body.Bytes())
	}
	return w.Code, f, c
}

func ptr(s string) *string { return &s }

// 邀请（AUTH-22，M1-02 验收 6）：outbox 行只有 staff_invitation_id、没有邮箱，链接加密保存并在投递后清除；
// 邮件语言取邀请人的 locale；审计不含邮箱。
func TestCreateInvitation(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	e.stepUp(t, super)
	w := e.do(req{method: "POST", path: "/v1/staff-invitations", as: super, sensitive: true,
		body: map[string]any{"email": "New.Ops@Example.com", "roles": []string{"support", "operator"}, "reason": "新同事"}})
	if w.Code != 201 {
		t.Fatalf("invite: %d %s", w.Code, w.Body)
	}
	var inv struct {
		ID        uuid.UUID `json:"id"`
		Email     string    `json:"email"`
		Roles     []string  `json:"roles"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	decode(t, w, &inv)
	if inv.Email != "new.ops@example.com" || !slices.Equal(inv.Roles, []string{"operator", "support"}) || !inv.ExpiresAt.Equal(e.clk.Now().Add(72*time.Hour)) {
		t.Fatalf("invitation = %+v", inv)
	}
	var account *uuid.UUID
	var vars string
	var secret []byte
	if err := e.pool.QueryRow(t.Context(), `SELECT account_id, variables::text, secret_variables_enc FROM notification_outbox
		WHERE staff_invitation_id = $1 AND template = 'staff_invitation'`, inv.ID).Scan(&account, &vars, &secret); err != nil {
		t.Fatal(err)
	}
	if account != nil || strings.Contains(vars, "@") || strings.Contains(vars, "token") || len(secret) == 0 {
		t.Fatalf("outbox row: account %v vars %s", account, vars)
	}
	var diff string
	if err := e.pool.QueryRow(t.Context(), `SELECT diff::text FROM audit_logs WHERE action = 'staff_invitation.create'`).Scan(&diff); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(diff, "@") {
		t.Fatalf("audit diff contains the email: %s", diff)
	}
	sent := e.deliver(t)
	if len(sent) != 1 || sent[0].To != "new.ops@example.com" || !strings.Contains(sent[0].Body, "#token=") {
		t.Fatalf("sent = %+v", sent)
	}
	if n := e.count(t, `SELECT count(*) FROM notification_outbox WHERE staff_invitation_id = $1 AND secret_variables_enc IS NOT NULL`, inv.ID); n != 0 {
		t.Fatal("secret variables not cleared after delivery (CONV-31)")
	}
	// 已有 pending 邀请、已是管理员：400 taken。
	for _, email := range []string{"new.ops@example.com", super.email} {
		w := e.do(req{method: "POST", path: "/v1/staff-invitations", as: super, sensitive: true,
			body: map[string]any{"email": email, "roles": []string{"support"}, "reason": "x"}})
		if f, c := fieldCode(t, w); w.Code != 400 || f != "email" || c != "taken" {
			t.Errorf("%s: %d %s", email, w.Code, w.Body)
		}
	}
	// 列表与详情。
	w = e.do(req{method: "GET", path: "/v1/staff-invitations?status=pending", as: super})
	if w.Code != 200 || !strings.Contains(w.Body.String(), inv.ID.String()) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if w := e.do(req{method: "GET", path: "/v1/staff-invitations?status=accepted", as: super}); strings.Contains(w.Body.String(), inv.ID.String()) {
		t.Fatal("pending invitation listed as accepted")
	}
	if w := e.do(req{method: "GET", path: "/v1/staff-invitations/" + inv.ID.String(), as: super}); w.Code != 200 {
		t.Fatalf("get: %d", w.Code)
	}
}

// 撤销邀请：只能撤销 pending（否则 409）；撤销后令牌无效，未投递的邮件不再投递。
func TestRevokeInvitation(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	e.stepUp(t, super)
	id, token := e.invite(t, super, "r@example.com", "support")
	revoke := func() int {
		return e.do(req{method: "DELETE", path: "/v1/staff-invitations/" + id.String(), as: super, sensitive: true,
			header: map[string]string{"Audit-Reason": "x"}}).Code
	}
	if c := revoke(); c != 204 {
		t.Fatalf("revoke: %d", c)
	}
	if c := revoke(); c != 409 {
		t.Fatalf("revoke again: %d", c)
	}
	if c, f, code := e.accept(t, token, ptr("new password 1")); c != 400 || f != "token" || code != "invalid_code" {
		t.Fatalf("accept revoked: %d %s %s", c, f, code)
	}
	// 撤销之后才到期投递的邮件不发出。
	id2 := e.invitation(t, super, "late@example.com", e.clk.Now().Add(time.Hour), "support")
	if err := e.sessions.Outbox.Enqueue(t.Context(), e.q(), notify.Message{StaffInvitationID: &id2, Template: notify.TemplateStaffInvitation,
		Vars: map[string]string{"hours": "72"}, Secrets: map[string]string{"link": "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(t.Context(), `UPDATE staff_invitations SET revoked_at = $1 WHERE id = $2`, e.clk.Now(), id2); err != nil {
		t.Fatal(err)
	}
	if sent := e.deliver(t); len(sent) != 0 {
		t.Fatalf("sent after revocation: %+v", sent)
	}
}

// 接受邀请的四种账号状态（AUTH-22，M1-02 验收 4）与令牌只能使用一次。
func TestAcceptInvitation(t *testing.T) {
	e := newEnv(t)
	super := e.staff(t, "superadmin")
	e.stepUp(t, super)

	t.Run("new account", func(t *testing.T) {
		_, token := e.invite(t, super, "fresh@example.com", "operator")
		if c, f, code := e.accept(t, token, nil); c != 400 || f != "password" || code != "required" {
			t.Fatalf("without password: %d %s %s", c, f, code)
		}
		if c, _, _ := e.accept(t, token, ptr("correct horse battery")); c != 200 {
			t.Fatalf("accept: %d", c)
		}
		if c, f, code := e.accept(t, token, ptr("correct horse battery")); c != 400 || f != "token" || code != "invalid_code" {
			t.Fatalf("second use: %d %s %s", c, f, code)
		}
		var verified *time.Time
		var id uuid.UUID
		if err := e.pool.QueryRow(t.Context(), `SELECT id, email_verified_at FROM accounts WHERE email = 'fresh@example.com'`).Scan(&id, &verified); err != nil || verified == nil {
			t.Fatalf("account %v verified %v", err, verified)
		}
		if n := e.count(t, `SELECT count(*) FROM proxy_credentials WHERE account_id = $1 AND device_id IS NULL`, id); n != 1 {
			t.Fatal("shared credential missing (AUTH-13)")
		}
		if n := e.count(t, `SELECT count(*) FROM outbox WHERE topic = 'credential.changed' AND payload->>'account_id' = $1`, id.String()); n != 1 {
			t.Fatal("credential.changed missing")
		}
		var diff string
		var actor uuid.UUID
		if err := e.pool.QueryRow(t.Context(), `SELECT diff::text, actor_id FROM audit_logs WHERE action = 'staff.create' AND target_id = $1`, id.String()).Scan(&diff, &actor); err != nil {
			t.Fatal(err)
		}
		if actor != id || !strings.Contains(diff, `"is_new_account": true`) || !strings.Contains(diff, "staff_invitation_id") {
			t.Fatalf("audit actor %v diff %s", actor, diff)
		}
		// 新管理员首次登录必须绑定 TOTP（AUTH-21）。
		w := e.do(req{method: "POST", path: "/v1/sessions", body: map[string]string{"email": "fresh@example.com", "password": "correct horse battery"}})
		if _, ok := problem(t, w)["totp_enrollment"]; w.Code != 401 || !ok {
			t.Fatalf("first login: %d %s", w.Code, w.Body)
		}
	})

	t.Run("existing verified", func(t *testing.T) {
		a := e.account(t, true)
		_, token := e.invite(t, super, a.email, "support")
		if c, _, _ := e.accept(t, token, ptr("ignored password")); c != 200 {
			t.Fatalf("accept: %d", c)
		}
		e.login(t, a) // 原密码与 TOTP 仍然有效
		if n := e.count(t, `SELECT count(*) FROM notification_outbox WHERE account_id = $1 AND template = 'staff_roles_changed'`, a.id); n != 1 {
			t.Fatal("staff_roles_changed not sent")
		}
	})

	t.Run("existing unverified", func(t *testing.T) {
		a := e.account(t, true)
		if _, err := e.pool.Exec(t.Context(), `UPDATE accounts SET email_verified_at = NULL WHERE id = $1`, a.id); err != nil {
			t.Fatal(err)
		}
		// 抢先注册者的会话。
		if _, err := e.pool.Exec(t.Context(), `INSERT INTO sessions (account_id, audience, refresh_token_hash, expires_at) VALUES ($1, 'client', $2, $3)`,
			a.id, uuid.NewString(), e.clk.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		_, token := e.invite(t, super, a.email, "support")
		if c, f, _ := e.accept(t, token, nil); c != 400 || f != "password" {
			t.Fatalf("without password: %d %s", c, f)
		}
		if c, _, _ := e.accept(t, token, ptr("the real owner pw")); c != 200 {
			t.Fatalf("accept: %d", c)
		}
		var hash string
		if err := e.pool.QueryRow(t.Context(), `SELECT password_hash FROM accounts WHERE id = $1`, a.id).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if ok, _ := password.Verify("the real owner pw", hash); !ok {
			t.Fatal("password not replaced")
		}
		if n := e.count(t, `SELECT count(*) FROM mfa_totp WHERE account_id = $1`, a.id); n != 0 {
			t.Fatal("TOTP of the unverified account not deleted")
		}
		if n := e.count(t, `SELECT count(*) FROM sessions WHERE account_id = $1 AND revoked_at IS NULL`, a.id); n != 0 {
			t.Fatal("sessions not revoked")
		}
		if n := e.count(t, `SELECT count(*) FROM accounts WHERE id = $1 AND email_verified_at IS NOT NULL`, a.id); n != 1 {
			t.Fatal("email not marked verified")
		}
	})

	t.Run("invalid states", func(t *testing.T) {
		sus := e.account(t, true)
		_, tokSus := e.invite(t, super, sus.email, "support")
		if _, err := e.pool.Exec(t.Context(), `UPDATE accounts SET status = 'suspended' WHERE id = $1`, sus.id); err != nil {
			t.Fatal(err)
		}
		if c, _, code := e.accept(t, tokSus, nil); c != 409 || code != "invalid_state" {
			t.Fatalf("suspended: %d %s", c, code)
		}
		already := e.account(t, true)
		_, tokAlready := e.invite(t, super, already.email, "support")
		if _, err := e.pool.Exec(t.Context(), `INSERT INTO account_roles (account_id, role) VALUES ($1, 'operator')`, already.id); err != nil {
			t.Fatal(err)
		}
		if c, _, code := e.accept(t, tokAlready, nil); c != 409 || code != "invalid_state" {
			t.Fatalf("already staff: %d %s", c, code)
		}
		if c, f, code := e.accept(t, "no-such-token", nil); c != 400 || f != "token" || code != "invalid_code" {
			t.Fatalf("unknown token: %d %s %s", c, f, code)
		}
	})

	t.Run("expired", func(t *testing.T) {
		_, token := e.invite(t, super, "late@example.com", "support")
		e.clk.Advance(InvitationTTL)
		if c, f, code := e.accept(t, token, ptr("correct horse battery")); c != 400 || f != "token" || code != "expired" {
			t.Fatalf("expired: %d %s %s", c, f, code)
		}
	})
}
