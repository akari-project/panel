// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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

	"github.com/akari-project/panel/server/internal/account"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/clientapi/gen"
	"github.com/akari-project/panel/server/internal/clientconfig"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/httpx"
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

type env struct {
	h        http.Handler
	pool     *pgxpool.Pool
	clk      *clock.Fake
	signer   *clientconfig.Signer
	tokens   *token.Keyring
	keys     *secretbox.Keyring
	rev      auth.Revocations
	server   *Server
	sessions *session.Service
	accounts *account.Service
	// verifyCalls 统计密码校验（argon2id）的调用次数（AUTH-09 验收）。
	verifyCalls atomic.Int32
	// ip 是本测试的客户端地址。各测试进程共享同一个 Valkey，按 IP 的限流计数互不干扰。
	ip string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	tokens, err := token.NewKeyring(clk, "1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	kvc := testkv.New(t)
	keys, err := secretbox.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := clientconfig.ParseKey("5:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{pool: testdb.New(t), clk: clk, tokens: tokens, keys: keys, rev: auth.Revocations{KV: kvc}, signer: signer}
	u := uuid.New()
	e.ip = netip.AddrFrom4([4]byte{10, u[0], u[1], u[2]}).String()
	limiter := ratelimit.Limiter{KV: kvc}
	// 测试用较小的 argon2id 参数；生产参数见 password.DefaultParams。
	fast := password.Params{MemoryKiB: 1024, Iterations: 1, Parallelism: 1}
	e.sessions = &session.Service{Pool: e.pool, KV: kvc, Clock: clk, Keys: keys, Tokens: tokens, Revocations: e.rev, Limiter: limiter,
		Verify: func(pw, encoded string) (bool, error) {
			e.verifyCalls.Add(1)
			return password.Verify(pw, encoded)
		}}
	outbox := notify.Outbox{Keys: keys, Clock: clk}
	e.sessions.Outbox = outbox
	e.sessions.MFA = &mfa.Service{Pool: e.pool, KV: kvc, Clock: clk, Keys: keys, Outbox: outbox, Issuer: "Akari",
		AfterRevoke: e.sessions.AfterRevoke}
	e.accounts = &account.Service{Pool: e.pool, Clock: clk, Keys: keys, Outbox: notify.Outbox{Keys: keys, Clock: clk}, Limiter: limiter,
		Password: fast, Invites: account.ReferralCodes{}, Captcha: account.NoCaptcha{}, PortalURL: "https://portal.example.com/",
		Revoke: e.sessions.RevokeAccount, AfterRevoke: e.sessions.AfterRevoke}
	d := Deps{
		Log: slog.New(slog.DiscardHandler), Clock: clk, Pool: e.pool, Tokens: tokens,
		Revocations: e.rev, Limiter: limiter, IdempotencyKey: []byte("k"), Accounts: e.accounts, Sessions: e.sessions,
		MFA:    e.sessions.MFA,
		Config: ConfigDeps{Signer: e.signer, AppName: "Akari", APIEndpoints: []string{"https://api.example.com", "https://api-backup.example.net/panel"}},
	}
	e.h = New(d)
	e.server = e.h.(*router).s
	return e
}

// stub 在路由上注册一个尚未实现的契约操作，记录处理器看到的认证主体。
func (e *env) stub(pattern string) *[]bool {
	var seen []bool
	e.h.(*router).mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		_, ok := auth.FromContext(r.Context())
		seen = append(seen, ok)
		w.WriteHeader(204)
	})
	return &seen
}

// account 建立一个账号并返回其 ID。
func (e *env) account(t *testing.T, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := e.pool.QueryRow(context.Background(),
		`INSERT INTO accounts (email, password_hash, referral_code, email_verified_at) VALUES ($1, 'x', $2, $3) RETURNING id`,
		email, strings.ToUpper(uuid.NewString()[:6]), t0).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) token(t *testing.T, account uuid.UUID, aud token.Audience) (string, uuid.UUID) {
	t.Helper()
	sid := uuid.New()
	tok, _, err := e.tokens.Issue(token.Claims{AccountID: account, SessionID: sid, Audience: aud})
	if err != nil {
		t.Fatal(err)
	}
	return tok, sid
}

func (e *env) get(path string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	// 与生产一致：经访问日志分配 request_id。
	httpx.AccessLog(slog.New(slog.DiscardHandler), e.clk, e.h).ServeHTTP(w, r)
	return w
}

func bearerAuth(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func problemCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type %q, body %s", ct, w.Body)
	}
	var p struct {
		Code      string `json:"code"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.RequestID == "" {
		t.Fatal("problem without request_id")
	}
	return p.Code
}

func TestUnknownRoutes(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/v1/nope", "/v1/plans"} { // 不存在；存在于契约但尚未实现
		w := e.get(path, nil)
		if w.Code != 404 || problemCode(t, w) != "not_found" {
			t.Errorf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest("DELETE", "/v1/me/nothing", nil)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Errorf("DELETE unknown: %d", w.Code)
	}
}

// AUTH-06、AUTH-08：Bearer 与 Cookie 两种方式；受众必须为 client；吊销与过期立即生效。
func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	id := e.account(t, "a@example.com")
	tok, sid := e.token(t, id, token.AudienceClient)
	consoleTok, _ := e.token(t, id, token.AudienceConsole)

	if w := e.get("/v1/me", nil); w.Code != 401 || problemCode(t, w) != "unauthenticated" {
		t.Fatalf("no token: %d %s", w.Code, w.Body)
	}
	if w := e.get("/v1/me", bearerAuth(tok)); w.Code != 200 {
		t.Fatalf("bearer: %d %s", w.Code, w.Body)
	}
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: AccessCookie, Value: tok}) }
	if w := e.get("/v1/me", cookie); w.Code != 200 {
		t.Fatalf("cookie: %d %s", w.Code, w.Body)
	}
	for name, mod := range map[string]func(*http.Request){
		"console audience": bearerAuth(consoleTok),
		"garbage":          bearerAuth("v4.public.garbage"),
		"basic scheme":     func(r *http.Request) { r.Header.Set("Authorization", "Basic "+tok) },
	} {
		w := e.get("/v1/me", mod)
		if w.Code != 401 || problemCode(t, w) != "unauthenticated" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}

	if err := e.rev.Revoke(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	if w := e.get("/v1/me", bearerAuth(tok)); w.Code != 401 {
		t.Fatalf("revoked session: %d", w.Code)
	}

	tok2, _ := e.token(t, id, token.AudienceClient)
	e.clk.Advance(token.TTL)
	if w := e.get("/v1/me", bearerAuth(tok2)); w.Code != 401 {
		t.Fatalf("expired token: %d", w.Code)
	}
}

// 无需认证的操作不读取令牌：残留的已吊销或已过期 Cookie 不能妨碍重新登录与刷新。
// 可选认证的操作持有无效令牌时按未认证处理。
func TestStaleTokenOnPublicAndOptional(t *testing.T) {
	e := newEnv(t)
	id := e.account(t, "stale@example.com")
	tok, sid := e.token(t, id, token.AudienceClient)
	if err := e.rev.Revoke(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	login := e.stub("POST /v1/oauth/device_authorization")
	plans := e.stub("GET /v1/plans")
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: AccessCookie, Value: tok}) }

	r := httptest.NewRequest("POST", "/v1/oauth/device_authorization", strings.NewReader(`{}`))
	cookie(r)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != 204 || len(*login) != 1 || (*login)[0] {
		t.Fatalf("public with revoked cookie: %d %s, seen %v", w.Code, w.Body, *login)
	}
	if w := e.get("/v1/plans", cookie); w.Code != 204 || len(*plans) != 1 || (*plans)[0] {
		t.Fatalf("optional with revoked cookie: %d, seen %v", w.Code, *plans)
	}
	valid, _ := e.token(t, id, token.AudienceClient)
	if w := e.get("/v1/plans", bearerAuth(valid)); w.Code != 204 || !(*plans)[1] {
		t.Fatalf("optional with valid token: %d, seen %v", w.Code, *plans)
	}
}

func TestGetMe(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.account(t, "Me@Example.com")
	tok, _ := e.token(t, id, token.AudienceClient)

	var me gen.Me
	w := e.get("/v1/me", bearerAuth(tok))
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil || w.Code != 200 {
		t.Fatalf("%d %s %v", w.Code, w.Body, err)
	}
	if me.Id != id || string(me.Email) != "Me@Example.com" || !me.IsEmailVerified || me.Status != gen.MeStatusActive ||
		me.IsMfaEnabled || len(me.MfaMethods) != 0 || me.EntitlementStatus != gen.EntitlementStatusNone || me.DeviceLimit != 1 || me.Timezone != "Asia/Shanghai" {
		t.Fatalf("me = %+v", me)
	}
	// 必填字段必须出现，即使为空（CONV-16 以外的契约要求）。
	var raw map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	for _, k := range []string{"timezone", "mfa_methods", "device_limit"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing %s in %s", k, w.Body)
		}
	}

	// 免费账号的设备上限取设置项 free_device_limit（AUTH-14）；启用 TOTP 后显示二次验证方式。
	if _, err := e.pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('free_device_limit', '3')`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO mfa_totp (account_id, secret_enc, recovery_hashes, enabled_at) VALUES ($1, '\x00', '{}', $2)`, id, t0); err != nil {
		t.Fatal(err)
	}
	w = e.get("/v1/me", bearerAuth(tok))
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if me.DeviceLimit != 3 || !me.IsMfaEnabled || len(me.MfaMethods) != 2 {
		t.Fatalf("me = %+v", me)
	}

	// 账号已不存在：视为未认证。
	if _, err := e.pool.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if w := e.get("/v1/me", bearerAuth(tok)); w.Code != 401 {
		t.Fatalf("deleted account: %d", w.Code)
	}
}

// API-04：无需认证的操作按 IP 限流，已认证的写操作按账号限流，读操作不限。
func TestLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pub := gen.Operation{Method: "POST", Auth: gen.AuthPublic}
	write := gen.Operation{Method: "POST", Auth: gen.AuthRequired}
	read := gen.Operation{Method: "GET", Auth: gen.AuthRequired}
	r := httptest.NewRequest("POST", "/v1/x", nil)
	r.RemoteAddr = netip.AddrPortFrom(netip.MustParseAddr("203.0.113.9"), 1234).String()
	p := auth.Principal{AccountID: uuid.New()}

	for i := int64(0); i < PublicRule.Limit; i++ {
		if err := e.server.limit(ctx, r, pub, false, auth.Principal{}); err != nil {
			t.Fatalf("public %d: %v", i, err)
		}
	}
	if err := e.server.limit(ctx, r, pub, false, auth.Principal{}); err == nil || !strings.Contains(err.Error(), "rate_limited") {
		t.Fatalf("public over limit: %v", err)
	}
	for i := int64(0); i < WriteRule.Limit; i++ {
		if err := e.server.limit(ctx, r, write, true, p); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := e.server.limit(ctx, r, write, true, p); err == nil {
		t.Fatal("write over limit allowed")
	}
	if err := e.server.limit(ctx, r, read, true, p); err != nil {
		t.Fatalf("read limited: %v", err)
	}
}

func TestIPSubject(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9":          "203.0.113.9",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
	} {
		if got := ipSubject(netip.MustParseAddr(in)); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	if ipSubject(netip.Addr{}) != "unknown" {
		t.Error("invalid addr")
	}
}

// 生成的操作表与契约一致：抽查认证方式与 Idempotency-Key（CONV-12 不接受该请求头的操作）。
func TestOperationTable(t *testing.T) {
	for pattern, want := range map[string]gen.Operation{
		"GET /v1/me":                      {Auth: gen.AuthRequired},
		"POST /v1/accounts":               {Auth: gen.AuthPublic, Idempotent: true},
		"POST /v1/sessions":               {Auth: gen.AuthPublic},
		"POST /v1/oauth/token":            {Auth: gen.AuthPublic},
		"POST /v1/accounts/verification":  {Auth: gen.AuthOptional, Idempotent: true},
		"POST /v1/me/mfa/totp":            {Auth: gen.AuthRequired},
		"POST /v1/me/mfa/totp/activation": {Auth: gen.AuthRequired},
		"GET /v1/plans":                   {Auth: gen.AuthOptional},
	} {
		got, ok := gen.Operations[pattern]
		if !ok || got.Auth != want.Auth || got.Idempotent != want.Idempotent || got.Pattern != pattern {
			t.Errorf("%s: %+v, want %+v", pattern, got, want)
		}
	}
}
