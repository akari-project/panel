// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/secretbox"
	"github.com/akari-project/panel/server/internal/testdb"
)

var t0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

func testKeys(t *testing.T) *secretbox.Keyring {
	t.Helper()
	k, err := secretbox.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type fakeSender struct {
	mu   sync.Mutex
	sent []Email
	err  error
}

func (f *fakeSender) Send(_ context.Context, m Email) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, m)
	return nil
}

type fixture struct {
	pool   *pgxpool.Pool
	clk    *clock.Fake
	out    Outbox
	d      *Deliverer
	sender *fakeSender
}

func newFixture(t *testing.T) *fixture {
	pool := testdb.New(t)
	clk := clock.NewFake(t0)
	keys := testKeys(t)
	s := &fakeSender{}
	return &fixture{
		pool: pool, clk: clk, sender: s,
		out: Outbox{Keys: keys, Clock: clk},
		d:   &Deliverer{Pool: pool, Keys: keys, Clock: clk, Sender: s, SiteName: "Akari"},
	}
}

func (f *fixture) account(t *testing.T, email, locale string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO accounts (email, referral_code, locale) VALUES ($1, $2, $3) RETURNING id`,
		email, strings.ToUpper(uuid.NewString()[:8]), locale).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) enqueue(t *testing.T, m Message) {
	t.Helper()
	if err := f.out.Enqueue(context.Background(), sqlc.New(f.pool), m); err != nil {
		t.Fatal(err)
	}
}

type row struct {
	attempts int32
	sent     bool
	failed   bool
	secret   []byte
	lastErr  *string
	next     time.Time
}

func (f *fixture) row(t *testing.T) row {
	t.Helper()
	var r row
	var sentAt, failedAt *time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT attempts, sent_at, failed_at, secret_variables_enc, last_error, next_attempt_at FROM notification_outbox`).
		Scan(&r.attempts, &sentAt, &failedAt, &r.secret, &r.lastErr, &r.next); err != nil {
		t.Fatal(err)
	}
	r.sent, r.failed = sentAt != nil, failedAt != nil
	return r
}

func TestEnqueueValidatesAndEncrypts(t *testing.T) {
	f := newFixture(t)
	id := f.account(t, "a@example.com", "zh-CN")
	q := sqlc.New(f.pool)
	ctx := context.Background()
	if err := f.out.Enqueue(ctx, q, Message{AccountID: id, Template: "nope"}); err == nil {
		t.Fatal("unknown template accepted")
	}
	if err := f.out.Enqueue(ctx, q, Message{AccountID: id, Template: TemplateEmailVerification, Vars: map[string]string{"evil": "x"}}); err == nil {
		t.Fatal("variable outside whitelist accepted")
	}
	f.enqueue(t, Message{AccountID: id, Template: TemplateEmailVerification, Vars: map[string]string{"minutes": "15"},
		Secrets: map[string]string{"code": "493817"}, RetryFor: 15 * time.Minute})
	var vars string
	var enc []byte
	var retryUntil time.Time
	if err := f.pool.QueryRow(ctx, `SELECT variables::text, secret_variables_enc, retry_until FROM notification_outbox`).Scan(&vars, &enc, &retryUntil); err != nil {
		t.Fatal(err)
	}
	// CONV-31：验证码不以明文保存；OPS-02：最长重试时间等于验证码有效期。
	if strings.Contains(vars, "493817") || bytes.Contains(enc, []byte("493817")) || len(enc) == 0 {
		t.Fatalf("code stored in plaintext: vars=%s", vars)
	}
	if !retryUntil.Equal(t0.Add(15 * time.Minute)) {
		t.Fatalf("retry_until = %v", retryUntil)
	}
}

func TestDeliverSuccessClearsSecrets(t *testing.T) {
	f := newFixture(t)
	id := f.account(t, "b@example.com", "en-US")
	f.enqueue(t, Message{AccountID: id, Template: TemplateEmailVerification, Locale: "en-US",
		Vars: map[string]string{"minutes": "15"}, Secrets: map[string]string{"code": "493817"}})
	n, err := f.d.RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(f.sender.sent) != 1 {
		t.Fatalf("sent %d", len(f.sender.sent))
	}
	m := f.sender.sent[0]
	if m.To != "b@example.com" || m.Subject != "Akari verification code" || !strings.Contains(m.Body, "493817") || !strings.Contains(m.Body, "15 minutes") {
		t.Fatalf("email = %+v", m)
	}
	if r := f.row(t); !r.sent || r.secret != nil || r.attempts != 1 {
		t.Fatalf("row = %+v", r)
	}
	// 已发送的消息不再投递。
	if n, _ := f.d.RunOnce(context.Background()); n != 0 {
		t.Fatalf("re-delivered %d", n)
	}
}

// 失败后指数退避；超过 retry_until 最终失败并清除秘密变量（OPS-02、CONV-31）。
func TestRetryAndFinalFailure(t *testing.T) {
	f := newFixture(t)
	id := f.account(t, "c@example.com", "zh-CN")
	f.enqueue(t, Message{AccountID: id, Template: TemplatePasswordReset, Vars: map[string]string{"minutes": "30"},
		Secrets: map[string]string{"link": "https://example.com/reset#token=abc"}, RetryFor: 2 * time.Minute})
	f.sender.err = &SMTPError{Stage: "rcpt", Code: 451}
	ctx := context.Background()

	if _, err := f.d.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	r := f.row(t)
	if r.sent || r.failed || r.attempts != 1 || !r.next.Equal(t0.Add(30*time.Second)) || r.lastErr == nil || *r.lastErr != "smtp_rcpt_4xx" || r.secret == nil {
		t.Fatalf("after 1st failure: %+v", r)
	}
	// 未到重试时间不投递。
	if n, _ := f.d.RunOnce(ctx); n != 0 {
		t.Fatal("retried early")
	}
	f.clk.Advance(30 * time.Second)
	_, _ = f.d.RunOnce(ctx)
	if r := f.row(t); r.attempts != 2 || !r.next.Equal(t0.Add(90*time.Second)) {
		t.Fatalf("after 2nd failure: %+v", r)
	}
	f.clk.Advance(time.Minute)
	_, _ = f.d.RunOnce(ctx)
	if r := f.row(t); !r.failed || r.secret != nil || r.attempts != 3 {
		t.Fatalf("after final failure: %+v", r)
	}
}

func TestNotConfiguredAndTemplateOverride(t *testing.T) {
	f := newFixture(t)
	id := f.account(t, "d@example.com", "zh-CN")
	ctx := context.Background()
	f.enqueue(t, Message{AccountID: id, Template: TemplateRegisterAttempt})
	f.sender.err = ErrNotConfigured
	_, _ = f.d.RunOnce(ctx)
	if r := f.row(t); r.lastErr == nil || *r.lastErr != "not_configured" || r.failed {
		t.Fatalf("row = %+v", r)
	}
	// 后台覆盖的模板优先于内置模板；不在白名单中的变量使整条消息失败而不是输出半成品。
	if _, err := f.pool.Exec(ctx, `INSERT INTO notification_templates (template, locale, channel, subject, body)
		VALUES ('register_attempt', 'zh-CN', 'email', '【{{site_name}}】注册提醒', '自定义正文')`); err != nil {
		t.Fatal(err)
	}
	f.sender.err = nil
	f.clk.Advance(time.Minute)
	_, _ = f.d.RunOnce(ctx)
	if len(f.sender.sent) != 1 || f.sender.sent[0].Subject != "【Akari】注册提醒" || f.sender.sent[0].Body != "自定义正文" {
		t.Fatalf("sent = %+v", f.sender.sent)
	}
}

func TestRender(t *testing.T) {
	tpl := builtin[TemplatePasswordReset]
	out, err := render(tpl, "{{site_name}} {{ link }} {{minutes}}", map[string]string{"site_name": "A", "link": "L", "minutes": "30"})
	if err != nil || out != "A L 30" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := render(tpl, "{{code}}", map[string]string{"code": "1"}); err == nil {
		t.Fatal("variable outside whitelist rendered")
	}
	if _, err := render(tpl, "{{link}}", nil); err == nil {
		t.Fatal("missing variable rendered")
	}
	for name := range builtin {
		for _, l := range []string{"zh-CN", "en"} {
			c := builtin[name].locales[l]
			vars := map[string]string{"site_name": "S", "code": "1", "minutes": "1", "link": "L", "remaining": "2", "hours": "72", "roles": "operator"}
			if _, err := render(builtin[name], c.subject+c.body, vars); err != nil {
				t.Errorf("%s/%s: %v", name, l, err)
			}
		}
	}
	for in, want := range map[string]string{"zh-CN": "zh-CN", "zh-TW": "zh-CN", "en-US": "en", "fr": "zh-CN", "": "zh-CN"} {
		if got := normalizeLocale(in); got != want {
			t.Errorf("normalizeLocale(%q) = %q", in, got)
		}
	}
}

func TestBackoff(t *testing.T) {
	for n, want := range map[int32]time.Duration{1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 8: time.Hour, 20: time.Hour} {
		if got := backoff(n); got != want {
			t.Errorf("backoff(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestErrorClass(t *testing.T) {
	for err, want := range map[error]string{
		ErrNotConfigured:                     "not_configured",
		&SMTPError{Stage: "connect"}:         "smtp_connect",
		&SMTPError{Stage: "rcpt", Code: 550}: "smtp_rcpt_5xx",
		errors.New("550 user@example.com"):   "error",
		context.DeadlineExceeded:             "timeout",
	} {
		if got := errorClass(err); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
}
