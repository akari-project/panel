// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go"

	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/mfa"
	"github.com/akari-project/panel/server/internal/notify"
	"github.com/akari-project/panel/server/internal/password"
	"github.com/akari-project/panel/server/internal/ratelimit"
	"github.com/akari-project/panel/server/internal/secretbox"
	"github.com/akari-project/panel/server/internal/session"
	"github.com/akari-project/panel/server/internal/testdb"
	"github.com/akari-project/panel/server/internal/testkv"
)

var t0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

// fast 是测试用的 argon2id 参数；生产参数见 password.DefaultParams。
var fast = password.Params{MemoryKiB: 1024, Iterations: 1, Parallelism: 1}

type env struct {
	h        http.Handler
	pool     *pgxpool.Pool
	kv       valkey.Client
	clk      *clock.Fake
	tokens   *token.Keyring
	keys     *secretbox.Keyring
	sessions *session.Service
	// verifyCalls 统计密码校验（argon2id）的调用次数（AUTH-09）。
	verifyCalls atomic.Int32
	// ip 是本测试的客户端地址：各测试共享同一个 Valkey，按 IP 的限流计数互不干扰。
	ip string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	tokens, err := token.NewKeyring(clk, "1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := secretbox.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{pool: testdb.New(t), kv: testkv.New(t), clk: clk, tokens: tokens, keys: keys}
	u := uuid.New()
	e.ip = netip.AddrFrom4([4]byte{10, u[0], u[1], u[2]}).String()
	rev := auth.Revocations{KV: e.kv}
	limiter := ratelimit.Limiter{KV: e.kv}
	outbox := notify.Outbox{Keys: keys, Clock: clk}
	e.sessions = &session.Service{Pool: e.pool, KV: e.kv, Clock: clk, Keys: keys, Tokens: tokens, Revocations: rev, Limiter: limiter,
		Outbox: outbox, Verify: func(pw, encoded string) (bool, error) {
			e.verifyCalls.Add(1)
			return password.Verify(pw, encoded)
		}}
	e.sessions.MFA = &mfa.Service{Pool: e.pool, KV: e.kv, Clock: clk, Keys: keys, Outbox: outbox, Issuer: "Akari Console",
		AfterRevoke: e.sessions.AfterRevoke}
	e.h = New(Deps{
		Log: slog.New(slog.DiscardHandler), Clock: clk, Pool: e.pool, Keys: keys, Tokens: tokens, Revocations: rev,
		Limiter: limiter, IdempotencyKey: []byte("k"), Sessions: e.sessions, Outbox: outbox, Password: fast,
		AdminURL: "https://console.example.com/",
	})
	return e
}

// admin 是测试中的管理员（或普通账号）及其登录状态。
type admin struct {
	id       uuid.UUID
	email    string
	password string
	secret   []byte // TOTP 密钥；为空表示未启用
	access   string
	refresh  string
	// assertion 是 step-up 取得的 Mfa-Assertion。
	assertion string
}

// account 建立一个邮箱已验证、有密码的账号，可选启用 TOTP 并授予角色。
func (e *env) account(t *testing.T, withTOTP bool, roles ...string) *admin {
	t.Helper()
	ctx := context.Background()
	a := &admin{email: "a-" + uuid.NewString()[:8] + "@example.com", password: "correct horse battery"}
	hash, err := password.Hash(a.password, fast)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO accounts (email, password_hash, referral_code, email_verified_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		a.email, hash, strings.ToUpper(uuid.NewString()[:8]), t0).Scan(&a.id); err != nil {
		t.Fatal(err)
	}
	if withTOTP {
		a.secret = bytes.Repeat([]byte{byte(a.id[15])}, mfa.SecretSize)
		enc, err := e.keys.Seal(a.secret, []byte("mfa_totp.secret_enc:"+a.id.String()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(ctx, `INSERT INTO mfa_totp (account_id, secret_enc, recovery_hashes, enabled_at) VALUES ($1, $2, '{}', $3)`,
			a.id, enc, t0); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range roles {
		if _, err := e.pool.Exec(ctx, `INSERT INTO account_roles (account_id, role) VALUES ($1, $2)`, a.id, r); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// staff 建立一个启用 TOTP 的管理员并登录。
func (e *env) staff(t *testing.T, roles ...string) *admin {
	t.Helper()
	a := e.account(t, true, roles...)
	e.login(t, a)
	return a
}

// code 前进一个时间步后返回当前的 TOTP 码（同一时间步内已用过的码不能再用，AUTH-11）。
func (e *env) code(a *admin) string {
	e.clk.Advance(mfa.Period)
	return mfa.Code(a.secret, mfa.Step(e.clk.Now()))
}

// login 通过接口完成两步登录，保存令牌 Cookie。
func (e *env) login(t *testing.T, a *admin) {
	t.Helper()
	w := e.do(req{method: "POST", path: "/v1/sessions", body: map[string]string{"email": a.email, "password": a.password}})
	if w.Code != 401 || problemCode(t, w) != "mfa_required" {
		t.Fatalf("login step 1: %d %s", w.Code, w.Body)
	}
	challenge := problem(t, w)["challenge_id"].(string)
	w = e.do(req{method: "POST", path: "/v1/sessions", body: map[string]string{"challenge_id": challenge, "totp_code": e.code(a)}})
	if w.Code != 201 {
		t.Fatalf("login step 2: %d %s", w.Code, w.Body)
	}
	e.takeCookies(t, a, w)
}

func (e *env) takeCookies(t *testing.T, a *admin, w *httptest.ResponseRecorder) {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case AccessCookie:
			a.access = c.Value
		case RefreshCookie:
			a.refresh = c.Value
		}
	}
	if a.access == "" || a.refresh == "" {
		t.Fatalf("missing token cookies: %v", w.Header().Values("Set-Cookie"))
	}
}

// stepUp 取得 Mfa-Assertion。
func (e *env) stepUp(t *testing.T, a *admin) {
	t.Helper()
	w := e.do(req{method: "POST", path: "/v1/staff/me/step-up", as: a, body: map[string]string{"totp_code": e.code(a)}})
	if w.Code != 201 {
		t.Fatalf("step-up: %d %s", w.Code, w.Body)
	}
	var out struct {
		MfaAssertion string `json:"mfa_assertion"`
	}
	decode(t, w, &out)
	a.assertion = out.MfaAssertion
}

type req struct {
	method, path string
	body         any // nil、string（原样发送）或将编码为 JSON 的值
	as           *admin
	header       map[string]string
	// sensitive 为 true 时带上 as 的 Mfa-Assertion。
	sensitive bool
}

func (e *env) do(r req) *httptest.ResponseRecorder {
	var body io.Reader
	switch b := r.body.(type) {
	case nil:
	case string:
		body = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		body = bytes.NewReader(raw)
	}
	hr := httptest.NewRequest(r.method, r.path, body)
	hr.RemoteAddr = e.ip + ":1234"
	if r.body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	if r.as != nil {
		hr.AddCookie(&http.Cookie{Name: AccessCookie, Value: r.as.access})
		if r.sensitive && r.as.assertion != "" {
			hr.Header.Set("Mfa-Assertion", r.as.assertion)
		}
	}
	for k, v := range r.header {
		hr.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, hr)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
}

func problem(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var p map[string]any
	decode(t, w, &p)
	return p
}

func problemCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	c, _ := problem(t, w)["code"].(string)
	return c
}

// fieldCode 返回 400 响应中第一项 errors 的 field 与 code。
func fieldCode(t *testing.T, w *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var p struct {
		Errors []struct{ Field, Code string } `json:"errors"`
	}
	decode(t, w, &p)
	if len(p.Errors) == 0 {
		return "", ""
	}
	return p.Errors[0].Field, p.Errors[0].Code
}

// count 返回 SQL 计数。
func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// auditActions 返回按时间顺序写入的审计动作。
func (e *env) auditActions(t *testing.T) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT action FROM audit_logs ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func (e *env) q() *sqlc.Queries { return sqlc.New(e.pool) }

// invitation 直接在数据库中建立一个邀请，返回其 ID。
func (e *env) invitation(t *testing.T, inviter *admin, email string, expires time.Time, roles ...string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO staff_invitations (email, token_hash, inviter_id, expires_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		email, uuid.NewString(), inviter.id, expires).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if _, err := e.pool.Exec(context.Background(), `INSERT INTO staff_invitation_roles (staff_invitation_id, role) VALUES ($1, $2)`, id, r); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// role 直接在数据库中建立自定义角色，返回其 ETag。
func (e *env) role(t *testing.T, name string, perms ...string) string {
	t.Helper()
	var updated time.Time
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO roles (name, permissions) VALUES ($1, $2) RETURNING updated_at`,
		name, perms).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	return roleETag(updated)
}
