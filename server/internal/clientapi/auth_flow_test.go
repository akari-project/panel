// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/account"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/httpx"
	"github.com/akari-project/panel/server/internal/session"
)

// req 发出请求；ua 为空时使用固定的 User-Agent。
type req struct {
	method, path, body, contentType, ua, ip string
	bearer                                  string
	cookies                                 []*http.Cookie
	// idempotencyKey 非空时作为 Idempotency-Key 请求头（CONV-12）。
	idempotencyKey string
}

func (e *env) do(r req) *httptest.ResponseRecorder {
	hr := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
	if r.contentType == "" {
		r.contentType = "application/json"
	}
	hr.Header.Set("Content-Type", r.contentType)
	hr.Header.Set("User-Agent", "Mozilla/5.0 test")
	if r.ua != "" {
		hr.Header.Set("User-Agent", r.ua)
	}
	hr.RemoteAddr = e.ip + ":4000"
	if r.ip != "" {
		hr.RemoteAddr = r.ip + ":4000"
	}
	if r.bearer != "" {
		hr.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.idempotencyKey != "" {
		hr.Header.Set("Idempotency-Key", r.idempotencyKey)
	}
	for _, c := range r.cookies {
		hr.AddCookie(c)
	}
	w := httptest.NewRecorder()
	httpx.AccessLog(slog.New(slog.DiscardHandler), e.clk, e.h).ServeHTTP(w, hr)
	return w
}

func post(path, body string) req { return req{method: "POST", path: path, body: body} }

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// secret 从通知队列取出最近一条指定模板消息的秘密变量（测试中代替收信）。
func (e *env) secret(t *testing.T, template, name string) string {
	t.Helper()
	var enc []byte
	if err := e.pool.QueryRow(context.Background(),
		`SELECT secret_variables_enc FROM notification_outbox WHERE template = $1 ORDER BY id DESC LIMIT 1`, template).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	pt, err := e.keys.Open(enc, []byte("notification_outbox.secret_variables_enc"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	_ = json.Unmarshal(pt, &m)
	return m[name]
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func fieldCode(t *testing.T, w *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var p struct {
		Code   string `json:"code"`
		Errors []struct{ Field, Code string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if len(p.Errors) == 0 {
		return p.Code, ""
	}
	return p.Errors[0].Field, p.Errors[0].Code
}

func (e *env) register(t *testing.T, email, pw string) {
	t.Helper()
	w := e.do(post("/v1/accounts", jsonBody(map[string]string{"email": email, "password": pw})))
	if w.Code != 202 {
		t.Fatalf("register %s: %d %s", email, w.Code, w.Body)
	}
}

var webDevice = map[string]any{"platform": "web"}

func (e *env) login(t *testing.T, email, pw string, device map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(post("/v1/sessions", jsonBody(map[string]any{"email": email, "password": pw, "device": device})))
}

type sessionBody struct {
	DeviceID         uuid.UUID `json:"device_id"`
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	CredentialStatus string    `json:"credential_status"`
}

func appDevice(t *testing.T) (map[string]any, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	return map[string]any{"platform": "ios", "model": "iPhone17,1", "app_version": "1.4.0",
		"public_key": base64.StdEncoding.EncodeToString(der)}, priv
}

func (e *env) appLogin(t *testing.T, email, pw string) sessionBody {
	t.Helper()
	d, _ := appDevice(t)
	w := e.login(t, email, pw, d)
	if w.Code != 201 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var s sessionBody
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	return s
}

func (e *env) refresh(t *testing.T, refresh, ua string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := e.do(req{method: "POST", path: "/v1/oauth/token", contentType: "application/x-www-form-urlencoded", ua: ua,
		body: url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}.Encode()})
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w, m
}

// 注册 → 验证码邮件入队 → 未登录提交邮箱与验证码 → 登录后 is_email_verified 为真（AUTH-01、AUTH-03、AUTH-13）。
func TestRegisterVerifyLogin(t *testing.T) {
	e := newEnv(t)
	e.register(t, "New.User@Example.com", "correct horse battery")
	if n := e.count(t, `SELECT count(*) FROM accounts WHERE email = 'new.user@example.com' AND email_verified_at IS NULL`); n != 1 {
		t.Fatalf("account not created with normalized email")
	}
	// 共用代理凭据与 credential.changed 事件（AUTH-13）。
	if n := e.count(t, `SELECT count(*) FROM proxy_credentials c JOIN accounts a ON a.id = c.account_id WHERE a.email = 'new.user@example.com' AND c.device_id IS NULL`); n != 1 {
		t.Fatal("shared credential missing")
	}
	if n := e.count(t, `SELECT count(*) FROM outbox WHERE topic = 'credential.changed'`); n != 1 {
		t.Fatal("credential.changed missing")
	}
	code := e.secret(t, "email_verification", "code")
	if len(code) != 6 {
		t.Fatalf("code %q", code)
	}
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	w := e.do(post("/v1/accounts/verification", jsonBody(map[string]string{"email": "new.user@example.com", "code": wrong})))
	if f, c := fieldCode(t, w); w.Code != 400 || f != "code" || c != "invalid_code" {
		t.Fatalf("wrong code: %d %s", w.Code, w.Body)
	}
	w = e.do(post("/v1/accounts/verification", jsonBody(map[string]string{"email": "NEW.user@example.com", "code": code})))
	if w.Code != 204 {
		t.Fatalf("verify: %d %s", w.Code, w.Body)
	}
	s := e.appLogin(t, "new.user@example.com", "correct horse battery")
	w = e.get("/v1/me", bearerAuth(s.AccessToken))
	if !strings.Contains(w.Body.String(), `"is_email_verified":true`) {
		t.Fatalf("me: %s", w.Body)
	}
	// 验证码只能使用一次。
	w = e.do(post("/v1/accounts/verification", jsonBody(map[string]string{"email": "new.user@example.com", "code": code})))
	if w.Code != 400 {
		t.Fatalf("reuse: %d", w.Code)
	}
}

// AUTH-01：邮箱已注册时返回与成功相同的 202，不创建账号，改为发送提醒。
func TestRegisterExistingEmail(t *testing.T) {
	e := newEnv(t)
	e.register(t, "dup@example.com", "correct horse battery")
	first := e.do(post("/v1/accounts", jsonBody(map[string]string{"email": "other@example.com", "password": "correct horse battery"})))
	second := e.do(post("/v1/accounts", jsonBody(map[string]string{"email": "DUP@example.com", "password": "another password"})))
	if second.Code != 202 || second.Body.String() != first.Body.String() {
		t.Fatalf("existing: %d %s vs %s", second.Code, second.Body, first.Body)
	}
	if n := e.count(t, `SELECT count(*) FROM accounts WHERE lower(email) = 'dup@example.com'`); n != 1 {
		t.Fatalf("accounts = %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM notification_outbox WHERE template = 'register_attempt'`); n != 1 {
		t.Fatal("register_attempt not enqueued")
	}
}

// AUTH-02：注册策略、邀请码与邮箱域名名单。
func TestRegistrationPolicy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	set := func(key, value string) {
		if _, err := e.pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value); err != nil {
			t.Fatal(err)
		}
	}
	body := func(email string, invite *string) string {
		m := map[string]any{"email": email, "password": "correct horse battery"}
		if invite != nil {
			m["invite_code"] = *invite
		}
		return jsonBody(m)
	}
	e.register(t, "referrer@example.com", "correct horse battery")
	var code string
	var referrer uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT referral_code, id FROM accounts WHERE email = 'referrer@example.com'`).Scan(&code, &referrer); err != nil {
		t.Fatal(err)
	}

	set("registration_policy", `"closed"`)
	if w := e.do(post("/v1/accounts", body("a@example.com", nil))); w.Code != 403 || problemCode(t, w) != "registration_closed" {
		t.Fatalf("closed: %d %s", w.Code, w.Body)
	}
	set("registration_policy", `"invite_only"`)
	if w := e.do(post("/v1/accounts", body("a@example.com", nil))); w.Code != 403 {
		t.Fatalf("invite_only without code: %d", w.Code)
	}
	bad := "NOPE2345"
	if w := e.do(post("/v1/accounts", body("a@example.com", &bad))); w.Code != 400 {
		t.Fatalf("invalid code: %d", w.Code)
	} else if f, c := fieldCode(t, w); f != "invite_code" || c != "invalid_code" {
		t.Fatalf("invalid code: %s %s", f, c)
	}
	lower := strings.ToLower(code)
	if w := e.do(post("/v1/accounts", body("invited@example.com", &lower))); w.Code != 202 {
		t.Fatalf("valid code: %d %s", w.Code, w.Body)
	}
	if n := e.count(t, `SELECT count(*) FROM accounts WHERE email = 'invited@example.com' AND referrer_id = $1`, referrer); n != 1 {
		t.Fatal("referrer not recorded")
	}

	set("registration_policy", `"open"`)
	set("email_domain_denylist", `["spam.example"]`)
	if w := e.do(post("/v1/accounts", body("x@SPAM.example", nil))); w.Code != 400 {
		t.Fatalf("denylist: %d", w.Code)
	} else if f, c := fieldCode(t, w); f != "email" || c != "not_allowed" {
		t.Fatalf("denylist: %s %s", f, c)
	}
	set("email_domain_allowlist", `["corp.example"]`)
	if w := e.do(post("/v1/accounts", body("x@other.example", nil))); w.Code != 400 {
		t.Fatalf("allowlist: %d", w.Code)
	}
	if w := e.do(post("/v1/accounts", body("x@corp.example", nil))); w.Code != 202 {
		t.Fatalf("allowlisted: %d %s", w.Code, w.Body)
	}
}

func TestRegisterValidation(t *testing.T) {
	e := newEnv(t)
	for body, want := range map[string][2]string{
		`{"email":"a@example.com","password":"short"}`:                                        {"password", "too_short"},
		`{"email":"not-an-email","password":"correct horse battery"}`:                         {"email", "invalid_format"},
		`{"email":"a@example.com","password":"correct horse battery","timezone":"Mars/Base"}`: {"timezone", "invalid_format"},
	} {
		w := e.do(post("/v1/accounts", body))
		if f, c := fieldCode(t, w); w.Code != 400 || f != want[0] || c != want[1] {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
}

// 验收 2（AUTH-09）：登录失败与账号不存在返回相同错误，两条路径都恰好调用一次哈希函数。
func TestLoginFailureIndistinguishable(t *testing.T) {
	e := newEnv(t)
	e.register(t, "real@example.com", "correct horse battery")

	e.verifyCalls.Store(0)
	wrong := e.login(t, "real@example.com", "wrong password!", webDevice)
	if e.verifyCalls.Load() != 1 {
		t.Fatalf("wrong password: %d hash calls", e.verifyCalls.Load())
	}
	e.verifyCalls.Store(0)
	missing := e.login(t, "missing@example.com", "wrong password!", webDevice)
	if e.verifyCalls.Load() != 1 {
		t.Fatalf("unknown email: %d hash calls", e.verifyCalls.Load())
	}
	strip := func(w *httptest.ResponseRecorder) string {
		var m map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		delete(m, "request_id")
		return jsonBody(m)
	}
	if wrong.Code != 401 || missing.Code != 401 || strip(wrong) != strip(missing) {
		t.Fatalf("responses differ: %d %s / %d %s", wrong.Code, wrong.Body, missing.Code, missing.Body)
	}
}

// AUTH-09：同一账号 5 次失败后冷却；冷却对不存在的邮箱同样生效，返回相同的 429；找回密码不受影响。
func TestLoginCooldown(t *testing.T) {
	e := newEnv(t)
	e.register(t, "cool@example.com", "correct horse battery")
	for _, email := range []string{"cool@example.com", "ghost@example.com"} {
		for i := range 5 {
			if w := e.login(t, email, "wrong password!", webDevice); w.Code != 401 {
				t.Fatalf("%s attempt %d: %d", email, i+1, w.Code)
			}
		}
		w := e.login(t, email, "correct horse battery", webDevice)
		if w.Code != 429 || problemCode(t, w) != "rate_limited" || w.Header().Get("Retry-After") == "" {
			t.Fatalf("%s during cooldown: %d %s", email, w.Code, w.Body)
		}
	}
	if w := e.do(post("/v1/password-resets", `{"email":"cool@example.com"}`)); w.Code != 202 {
		t.Fatalf("reset during cooldown: %d", w.Code)
	}
}

// AUTH-08、AUTH-10：web 登录只注册 web 设备，令牌以 __Host- Cookie 下发，响应体不含令牌。
func TestWebLoginCookies(t *testing.T) {
	e := newEnv(t)
	e.register(t, "web@example.com", "correct horse battery")
	w := e.login(t, "web@example.com", "correct horse battery", webDevice)
	if w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var s sessionBody
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	if s.AccessToken != "" || s.RefreshToken != "" || s.CredentialStatus != "web_device" {
		t.Fatalf("body = %s", w.Body)
	}
	cookies := w.Result().Cookies()
	byName := map[string]*http.Cookie{}
	for _, c := range cookies {
		byName[c.Name] = c
		if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
			t.Errorf("cookie %s attributes: %+v", c.Name, c)
		}
	}
	access, refresh := byName[AccessCookie], byName[RefreshCookie]
	if access == nil || refresh == nil {
		t.Fatalf("cookies = %v", cookies)
	}
	if w := e.get("/v1/me", func(r *http.Request) { r.AddCookie(access) }); w.Code != 200 {
		t.Fatalf("me with cookie: %d", w.Code)
	}
	if n := e.count(t, `SELECT count(*) FROM proxy_credentials WHERE device_id = $1`, s.DeviceID); n != 0 {
		t.Fatal("web device got a credential")
	}
	// 浏览器刷新：refresh_token 由 Cookie 携带，新令牌同样以 Cookie 下发，响应体不含令牌。
	r := e.do(req{method: "POST", path: "/v1/oauth/token", contentType: "application/x-www-form-urlencoded",
		body: "grant_type=refresh_token", cookies: []*http.Cookie{refresh}})
	var pair map[string]any
	_ = json.Unmarshal(r.Body.Bytes(), &pair)
	_, hasAccess := pair["access_token"]
	_, hasRefresh := pair["refresh_token"]
	if r.Code != 200 || hasAccess || hasRefresh || pair["token_type"] != "Bearer" || len(r.Result().Cookies()) != 2 || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("web refresh: %d %s %v", r.Code, r.Body, r.Header())
	}
}

func TestAppLoginDevice(t *testing.T) {
	e := newEnv(t)
	e.register(t, "app@example.com", "correct horse battery")
	w := e.login(t, "app@example.com", "correct horse battery", map[string]any{"platform": "ios"})
	if f, c := fieldCode(t, w); w.Code != 400 || f != "device.public_key" || c != "required" {
		t.Fatalf("missing public key: %d %s", w.Code, w.Body)
	}
	d, priv := appDevice(t)
	s := e.appLogin(t, "app@example.com", "correct horse battery")
	if s.AccessToken == "" || s.RefreshToken == "" || s.CredentialStatus != "entitlement_inactive" {
		t.Fatalf("app login = %+v", s)
	}
	// 设备复用（AUTH-10）：带 device_id 与 nonce 签名登录，沿用原设备记录。
	first := e.login(t, "app@example.com", "correct horse battery", d)
	var one sessionBody
	_ = json.Unmarshal(first.Body.Bytes(), &one)
	newNonce := func() string {
		nw := e.do(post("/v1/sessions/nonces", ""))
		var n struct{ Nonce string }
		_ = json.Unmarshal(nw.Body.Bytes(), &n)
		if nw.Code != 201 || len(n.Nonce) != 43 || nw.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("nonce: %d %s", nw.Code, nw.Body)
		}
		return n.Nonce
	}
	sign := func(msg []byte) string { return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg)) }
	loginWith := func(proof map[string]string) *httptest.ResponseRecorder {
		d["device_id"], d["device_proof"] = one.DeviceID, proof
		return e.login(t, "app@example.com", "correct horse battery", d)
	}
	keyTaken := func(w *httptest.ResponseRecorder) bool {
		f, c := fieldCode(t, w)
		return w.Code == 400 && f == "device.public_key" && c == "not_allowed"
	}

	// 签名对象为 akari-device-proof-v1|<device_id>|<nonce>（AUTH-10）。
	n := newNonce()
	var two sessionBody
	reuse := loginWith(map[string]string{"nonce": n, "signature": sign(session.ProofMessage(one.DeviceID, n))})
	_ = json.Unmarshal(reuse.Body.Bytes(), &two)
	if reuse.Code != 201 || two.DeviceID != one.DeviceID {
		t.Fatalf("device not reused: %d %s", reuse.Code, reuse.Body)
	}
	// nonce 只能使用一次；未通过证明时不能以同一公钥注册新设备（同一账号的设备公钥不得重复）。
	if w := loginWith(map[string]string{"nonce": n, "signature": sign(session.ProofMessage(one.DeviceID, n))}); !keyTaken(w) {
		t.Fatalf("replayed nonce: %d %s", w.Code, w.Body)
	}
	// 旧格式（只签 nonce）与扫码批准的签名对象都不被接受（域分隔）。
	n = newNonce()
	if w := loginWith(map[string]string{"nonce": n, "signature": sign([]byte(n))}); !keyTaken(w) {
		t.Fatalf("nonce-only signature: %d %s", w.Code, w.Body)
	}
	n = newNonce()
	if w := loginWith(map[string]string{"nonce": n, "signature": sign([]byte("akari-device-link-approval-v1|" + one.DeviceID.String() + "|" + n))}); !keyTaken(w) {
		t.Fatalf("approval signature accepted as device proof: %d %s", w.Code, w.Body)
	}
}

// 验收 1（AUTH-07）：刷新令牌每次使用即轮换；已轮换的令牌再次出现时吊销整条会话链。
func TestRefreshReuseRevokesChain(t *testing.T) {
	e := newEnv(t)
	e.register(t, "chain@example.com", "correct horse battery")
	s := e.appLogin(t, "chain@example.com", "correct horse battery")

	w, p1 := e.refresh(t, s.RefreshToken, "")
	if w.Code != 200 || p1["refresh_token"] == s.RefreshToken || p1["token_type"] != "Bearer" {
		t.Fatalf("rotate: %d %s", w.Code, w.Body)
	}
	_, p2 := e.refresh(t, p1["refresh_token"].(string), "")
	latest := p2["access_token"].(string)
	if w := e.get("/v1/me", bearerAuth(latest)); w.Code != 200 {
		t.Fatalf("new access token: %d", w.Code)
	}

	e.clk.Advance(session.RetryWindow + time.Second)
	w, m := e.refresh(t, s.RefreshToken, "")
	if w.Code != 400 || m["error"] != "invalid_grant" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("reuse: %d %s", w.Code, w.Body)
	}
	// 整条链被吊销：最新的刷新令牌与访问令牌都失效。
	if w, m := e.refresh(t, p2["refresh_token"].(string), ""); w.Code != 400 || m["error"] != "invalid_grant" {
		t.Fatalf("latest refresh after reuse: %d %s", w.Code, w.Body)
	}
	if w := e.get("/v1/me", bearerAuth(latest)); w.Code != 401 {
		t.Fatalf("latest access after reuse: %d", w.Code)
	}
	if n := e.count(t, `SELECT count(*) FROM sessions WHERE revoked_at IS NULL`); n != 0 {
		t.Fatalf("%d sessions still active", n)
	}
}

// AUTH-07 例外：10 秒内同一 IP 前缀与 User-Agent 的重试返回同一新令牌对；指纹不同则吊销整条链。
func TestRefreshRetryWindow(t *testing.T) {
	e := newEnv(t)
	e.register(t, "retry@example.com", "correct horse battery")
	s := e.appLogin(t, "retry@example.com", "correct horse battery")
	_, first := e.refresh(t, s.RefreshToken, "")
	e.clk.Advance(5 * time.Second)
	w, again := e.refresh(t, s.RefreshToken, "")
	if w.Code != 200 || again["refresh_token"] != first["refresh_token"] || again["access_token"] != first["access_token"] {
		t.Fatalf("retry: %d %s", w.Code, w.Body)
	}
	if w, _ := e.refresh(t, s.RefreshToken, "OtherAgent/1.0"); w.Code != 400 {
		t.Fatalf("different user agent: %d", w.Code)
	}
	if w, _ := e.refresh(t, first["refresh_token"].(string), ""); w.Code != 400 {
		t.Fatal("chain not revoked after fingerprint mismatch")
	}
}

// 缓存未命中（如 Valkey 丢失数据）：窗口内指纹一致时返回 invalid_grant，不吊销会话链（ADR 0017）。
func TestRefreshRetryCacheMiss(t *testing.T) {
	e := newEnv(t)
	e.register(t, "miss@example.com", "correct horse battery")
	s := e.appLogin(t, "miss@example.com", "correct horse battery")
	_, first := e.refresh(t, s.RefreshToken, "")
	kv := e.sessions.KV
	if err := kv.Do(context.Background(), kv.B().Del().Key(session.RetryCacheKey(s.RefreshToken)).Build()).Error(); err != nil {
		t.Fatal(err)
	}
	if w, m := e.refresh(t, s.RefreshToken, ""); w.Code != 400 || m["error"] != "invalid_grant" {
		t.Fatalf("cache miss: %d %s", w.Code, w.Body)
	}
	if w, _ := e.refresh(t, first["refresh_token"].(string), ""); w.Code != 200 {
		t.Fatalf("chain revoked on cache miss: %d %s", w.Code, w.Body)
	}
}

// AUTH-07：空闲 30 天与绝对 90 天失效。
func TestRefreshExpiry(t *testing.T) {
	e := newEnv(t)
	e.register(t, "exp@example.com", "correct horse battery")
	s := e.appLogin(t, "exp@example.com", "correct horse battery")
	e.clk.Advance(session.ClientIdle)
	if w, _ := e.refresh(t, s.RefreshToken, ""); w.Code != 400 {
		t.Fatalf("idle expiry: %d", w.Code)
	}

	s = e.appLogin(t, "exp@example.com", "correct horse battery")
	tok := s.RefreshToken
	for range 3 { // 每 29 天刷新一次，共 87 天
		e.clk.Advance(29 * 24 * time.Hour)
		w, m := e.refresh(t, tok, "")
		if w.Code != 200 {
			t.Fatalf("refresh within idle window: %d %s", w.Code, w.Body)
		}
		tok = m["refresh_token"].(string)
	}
	e.clk.Advance(3 * 24 * time.Hour)
	if w, _ := e.refresh(t, tok, ""); w.Code != 400 {
		t.Fatalf("absolute expiry: %d", w.Code)
	}
}

// AUTH-10：登出吊销本会话、本设备及其代理凭据。
func TestLogout(t *testing.T) {
	e := newEnv(t)
	e.register(t, "bye@example.com", "correct horse battery")
	s := e.appLogin(t, "bye@example.com", "correct horse battery")
	w := e.do(req{method: "DELETE", path: "/v1/sessions/current", bearer: s.AccessToken})
	if w.Code != 204 {
		t.Fatalf("logout: %d %s", w.Code, w.Body)
	}
	if w := e.get("/v1/me", bearerAuth(s.AccessToken)); w.Code != 401 {
		t.Fatalf("access after logout: %d", w.Code)
	}
	if w, _ := e.refresh(t, s.RefreshToken, ""); w.Code != 400 {
		t.Fatalf("refresh after logout: %d", w.Code)
	}
	if n := e.count(t, `SELECT count(*) FROM devices WHERE id = $1 AND revoked_at IS NOT NULL`, s.DeviceID); n != 1 {
		t.Fatal("device not revoked")
	}
}

// AUTH-04：找回密码链接只存哈希，30 分钟有效，只能使用一次；重置后吊销全部会话；旧链接在新请求后作废。
func TestPasswordReset(t *testing.T) {
	e := newEnv(t)
	e.register(t, "forgot@example.com", "correct horse battery")
	s := e.appLogin(t, "forgot@example.com", "correct horse battery")

	if w := e.do(post("/v1/password-resets", `{"email":"nobody@example.com"}`)); w.Code != 202 {
		t.Fatalf("unknown email: %d", w.Code)
	}
	if w := e.do(post("/v1/password-resets", `{"email":"forgot@example.com"}`)); w.Code != 202 {
		t.Fatalf("request: %d", w.Code)
	}
	link := e.secret(t, "password_reset", "link")
	if !strings.HasPrefix(link, "https://portal.example.com/reset-password#token=") {
		t.Fatalf("link = %q", link)
	}
	old := link[strings.Index(link, "=")+1:]
	if n := e.count(t, `SELECT count(*) FROM verification_codes WHERE code_hash LIKE '%' || $1 || '%'`, old); n != 0 {
		t.Fatal("token stored in plaintext")
	}
	_ = e.do(post("/v1/password-resets", `{"email":"forgot@example.com"}`))
	token := e.secret(t, "password_reset", "link")
	token = token[strings.Index(token, "=")+1:]

	confirm := func(tok, pw string) *httptest.ResponseRecorder {
		return e.do(post("/v1/password-resets/confirmation", jsonBody(map[string]string{"token": tok, "new_password": pw})))
	}
	if w := confirm(old, "a brand new password"); w.Code != 400 {
		t.Fatalf("superseded link: %d", w.Code)
	}
	if w := confirm(token, "a brand new password"); w.Code != 204 {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	if w := e.get("/v1/me", bearerAuth(s.AccessToken)); w.Code != 401 {
		t.Fatalf("session survived reset: %d", w.Code)
	}
	if w := confirm(token, "yet another password"); w.Code != 400 {
		t.Fatalf("reused link: %d", w.Code)
	}
	if w := e.login(t, "forgot@example.com", "a brand new password", webDevice); w.Code != 201 {
		t.Fatalf("login with new password: %d", w.Code)
	}

	// 每个邮箱每小时 3 次（API-04），过期用例换一个账号。
	e.register(t, "forgot2@example.com", "correct horse battery")
	_ = e.do(post("/v1/password-resets", `{"email":"forgot2@example.com"}`))
	late := e.secret(t, "password_reset", "link")
	e.clk.Advance(account.ResetTTL)
	if w := confirm(late[strings.Index(late, "=")+1:], "too late password"); w.Code != 400 {
		t.Fatalf("expired link: %d", w.Code)
	} else if f, c := fieldCode(t, w); f != "token" || c != "expired" {
		t.Fatalf("expired link: %s %s", f, c)
	}
}

// AUTH-03：验证码 5 次错误后作废；15 分钟过期；重新发送每分钟 1 次，发送新码后旧码作废。
func TestVerificationLimits(t *testing.T) {
	e := newEnv(t)
	e.register(t, "limits@example.com", "correct horse battery")
	code := e.secret(t, "email_verification", "code")
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	body := func(c string) string { return jsonBody(map[string]string{"email": "limits@example.com", "code": c}) }
	for range 4 {
		_ = e.do(post("/v1/accounts/verification", body(wrong)))
	}
	// 未登录时次数用完与过期也返回 invalid_code，不透露邮箱已注册且未验证（AUTH-01 防枚举）。
	w := e.do(post("/v1/accounts/verification", body(wrong)))
	if _, c := fieldCode(t, w); c != "invalid_code" {
		t.Fatalf("5th wrong attempt: %s", w.Body)
	}
	if w := e.do(post("/v1/accounts/verification", body(code))); w.Code != 400 {
		t.Fatal("exhausted code accepted")
	}

	if w := e.do(post("/v1/accounts/verification/resend", `{"email":"limits@example.com"}`)); w.Code != 202 {
		t.Fatalf("resend: %d %s", w.Code, w.Body)
	}
	if w := e.do(post("/v1/accounts/verification/resend", `{"email":"limits@example.com"}`)); w.Code != 429 {
		t.Fatalf("second resend within a minute: %d", w.Code)
	}
	fresh := e.secret(t, "email_verification", "code")
	e.clk.Advance(account.CodeTTL)
	if w := e.do(post("/v1/accounts/verification", body(fresh))); w.Code != 400 {
		t.Fatal("expired code accepted")
	} else if _, c := fieldCode(t, w); c != "invalid_code" {
		t.Fatalf("expired: %s", w.Body)
	}
	// 未注册的邮箱同样返回 202（AUTH-01 防枚举）。
	if w := e.do(post("/v1/accounts/verification/resend", `{"email":"unknown@example.com"}`)); w.Code != 202 {
		t.Fatalf("unknown email resend: %d", w.Code)
	}
}

// 验收 3（AUTH-03）：未验证邮箱不能下单。下单在 M1-06 实现，届时在同一事务中调用本检查。
func TestRequireVerifiedEmail(t *testing.T) {
	e := newEnv(t)
	e.register(t, "unverified@example.com", "correct horse battery")
	var id uuid.UUID
	_ = e.pool.QueryRow(context.Background(), `SELECT id FROM accounts WHERE email = 'unverified@example.com'`).Scan(&id)
	q := sqlc.New(e.pool)
	if err := account.RequireVerifiedEmail(context.Background(), q, id); err != account.ErrEmailUnverified {
		t.Fatalf("unverified: %v", err)
	}
	code := e.secret(t, "email_verification", "code")
	_ = e.do(post("/v1/accounts/verification", jsonBody(map[string]string{"email": "unverified@example.com", "code": code})))
	if err := account.RequireVerifiedEmail(context.Background(), q, id); err != nil {
		t.Fatalf("verified: %v", err)
	}
}

// CONV-12：处理器内部的限流（AUTH-09）返回的 429 不缓存。等待 Retry-After 后用原键重试原请求必须重新执行，
// 而不是在 24 小时内重放 429。
func TestIdempotencyHandlerRateLimitNotCached(t *testing.T) {
	e := newEnv(t)
	email := "resend-" + uuid.NewString() + "@example.com"
	resend := func(key string) *httptest.ResponseRecorder {
		r := post("/v1/accounts/verification/resend", jsonBody(map[string]string{"email": email}))
		r.idempotencyKey = key
		return e.do(r)
	}
	if w := resend(uuid.NewString()); w.Code != 202 {
		t.Fatalf("first resend: %d %s", w.Code, w.Body)
	}
	key := uuid.NewString()
	if w := resend(key); w.Code != 429 || problemCode(t, w) != "rate_limited" {
		t.Fatalf("second resend within a minute: %d %s", w.Code, w.Body)
	}
	// 相当于等待 Retry-After 使每分钟的限额恢复。
	if err := e.accounts.Limiter.Reset(context.Background(), account.ResendPerMin, email); err != nil {
		t.Fatal(err)
	}
	if w := resend(key); w.Code != 202 {
		t.Fatalf("retry with the same key after Retry-After: %d %s", w.Code, w.Body)
	}
	// 成功结果按常规缓存：同键再次请求重放 202，不再计入限流。
	if w := resend(key); w.Code != 202 {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
}
