// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// invitation 建立一个由 inviter 发出、expires 失效的邀请。
func (f *fixture) invitation(t *testing.T, email string, inviter uuid.UUID, expires time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO staff_invitations (email, token_hash, inviter_id, expires_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		email, uuid.NewString(), inviter, expires).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// 邀请邮件的收件人可能没有账号：outbox 只保存 staff_invitation_id，投递时从邀请读取地址（AUTH-22、CONV-29）。
func TestInvitationEmail(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	inviter := f.account(t, "root@example.com", "en")
	inv := f.invitation(t, "new.ops@example.com", inviter, t0.Add(72*time.Hour))
	f.enqueue(t, Message{StaffInvitationID: &inv, Template: TemplateStaffInvitation, Locale: "en",
		Vars: map[string]string{"hours": "72"}, Secrets: map[string]string{"link": "https://console.example.com/accept-invitation#token=abc"},
		RetryFor: 72 * time.Hour})

	var account *uuid.UUID
	var vars string
	if err := f.pool.QueryRow(ctx, `SELECT account_id, variables::text FROM notification_outbox`).Scan(&account, &vars); err != nil {
		t.Fatal(err)
	}
	if account != nil || strings.Contains(vars, "@") || strings.Contains(vars, "token") {
		t.Fatalf("outbox row holds account %v, variables %s", account, vars)
	}
	if n, err := f.d.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	if len(f.sender.sent) != 1 || f.sender.sent[0].To != "new.ops@example.com" ||
		!strings.Contains(f.sender.sent[0].Body, "#token=abc") {
		t.Fatalf("sent %+v", f.sender.sent)
	}
	if r := f.row(t); !r.sent || r.secret != nil {
		t.Fatalf("row %+v", r)
	}
}

// 投递时邀请已不是 pending（已撤销、已接受或已过期）：不再投递，按最终失败处理并清除秘密变量。
func TestInvitationEmailClosed(t *testing.T) {
	for name, close := range map[string]string{
		"revoked":  `UPDATE staff_invitations SET revoked_at = $1`,
		"accepted": `UPDATE staff_invitations SET accepted_at = $1`,
		"expired":  `UPDATE staff_invitations SET expires_at = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			inviter := f.account(t, "root@example.com", "en")
			inv := f.invitation(t, "new.ops@example.com", inviter, t0.Add(72*time.Hour))
			f.enqueue(t, Message{StaffInvitationID: &inv, Template: TemplateStaffInvitation,
				Vars: map[string]string{"hours": "72"}, Secrets: map[string]string{"link": "x"}})
			if _, err := f.pool.Exec(ctx, close, t0); err != nil {
				t.Fatal(err)
			}
			if _, err := f.d.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			r := f.row(t)
			if len(f.sender.sent) != 0 || !r.failed || r.secret != nil || r.lastErr == nil || *r.lastErr != "invitation_closed" {
				t.Fatalf("sent %d, row %+v", len(f.sender.sent), r)
			}
		})
	}
}

func TestEnqueueRecipient(t *testing.T) {
	f := newFixture(t)
	q := sqlc.New(f.pool)
	id := f.account(t, "a@example.com", "zh-CN")
	inv := f.invitation(t, "b@example.com", id, t0.Add(time.Hour))
	for name, m := range map[string]Message{
		"none": {Template: TemplateStaffRolesChanged},
		"both": {AccountID: id, StaffInvitationID: &inv, Template: TemplateStaffRolesChanged},
	} {
		if err := f.out.Enqueue(context.Background(), q, m); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	// 数据库同样保证至多一个收件人（spec/03 3.6）。
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO notification_outbox (account_id, staff_invitation_id, channel, template, locale, variables, next_attempt_at, retry_until)
		 VALUES ($1, $2, 'email', 'x', 'en', '{}', $3, $3)`, id, inv, t0); err == nil {
		t.Error("CHECK notification_outbox_one_recipient not enforced")
	}
}
