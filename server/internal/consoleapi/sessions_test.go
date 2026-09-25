// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"encoding/base32"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akari-project/panel/server/internal/mfa"
	"github.com/akari-project/panel/server/internal/session"
)

func (e *env) refreshReq(a *admin) *httptest.ResponseRecorder {
	hr := httptest.NewRequest("POST", "/v1/oauth/token", strings.NewReader(url.Values{"grant_type": {"refresh_token"}}.Encode()))
	hr.RemoteAddr = e.ip + ":1234"
	hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hr.AddCookie(&http.Cookie{Name: RefreshCookie, Value: a.refresh})
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, hr)
	return w
}

// 管理员登录（AUTH-20、AUTH-21）：两步完成，令牌只在 Cookie 中，受众为 console；登录成功写审计 session.create。
func TestConsoleLogin(t *testing.T) {
	e := newEnv(t)
	a := e.account(t, true, "operator")
	e.login(t, a)
	if w := e.do(req{method: "GET", path: "/v1/staff/me", as: a}); w.Code != 200 {
		t.Fatalf("staff/me: %d %s", w.Code, w.Body)
	} else {
		var me struct {
			Roles        []string `json:"roles"`
			Permissions  []string `json:"permissions"`
			IsSuperadmin bool     `json:"is_superadmin"`
			HasTotp      bool     `json:"has_totp"`
		}
		decode(t, w, &me)
		if !slices.Equal(me.Roles, []string{"operator"}) || me.IsSuperadmin || !me.HasTotp || !slices.Contains(me.Permissions, "settings.read") {
			t.Fatalf("me = %+v", me)
		}
	}
	if got := e.auditActions(t); !slices.Equal(got, []string{"session.create"}) {
		t.Fatalf("audit = %v", got)
	}
	var device *string
	if err := e.pool.QueryRow(t.Context(), `SELECT device_id::text FROM sessions WHERE account_id = $1`, a.id).Scan(&device); err != nil || device != nil {
		t.Fatalf("console session device = %v, %v", device, err)
	}
	// 客户端接口的访问令牌不能用于管理接口（见 TestConsoleAuthentication）；管理令牌同样不能反向使用，由 clientapi 的受众校验保证。
}

// 密码错误与邮箱不存在：相同的 401，都恰好执行一次 argon2id（AUTH-09）；失败不写审计（AUTH-18）。
func TestConsoleLoginFailureIndistinguishable(t *testing.T) {
	e := newEnv(t)
	a := e.account(t, true, "operator")
	var bodies []map[string]any
	for _, email := range []string{a.email, "nobody@example.com"} {
		e.verifyCalls.Store(0)
		w := e.do(req{method: "POST", path: "/v1/sessions", body: map[string]string{"email": email, "password": "wrong password!"}})
		if w.Code != 401 || problemCode(t, w) != "unauthenticated" || e.verifyCalls.Load() != 1 {
			t.Fatalf("%s: %d %s, %d hashes", email, w.Code, w.Body, e.verifyCalls.Load())
		}
		p := problem(t, w)
		delete(p, "request_id")
		bodies = append(bodies, p)
	}
	if !reflect.DeepEqual(bodies[0], bodies[1]) {
		t.Fatalf("responses differ: %v vs %v", bodies[0], bodies[1])
	}
	if n := e.count(t, `SELECT count(*) FROM audit_logs`); n != 0 {
		t.Fatalf("%d audit rows after failed logins", n)
	}
}

// 密码正确但不是管理员：403 forbidden；账号暂停：403 account_suspended。
func TestConsoleLoginNotStaff(t *testing.T) {
	e := newEnv(t)
	plain := e.account(t, true)
	w := e.do(req{method: "POST", path: "/v1/sessions", body: map[string]string{"email": plain.email, "password": plain.password}})
	if w.Code != 403 || problemCode(t, w) != "forbidden" {
		t.Fatalf("not staff: %d %s", w.Code, w.Body)
	}
	sus := e.account(t, true, "operator")
	if _, err := e.pool.Exec(t.Context(), `UPDATE accounts SET status = 'suspended' WHERE id = $1`, sus.id); err != nil {
		t.Fatal(err)
	}
	w = e.do(req{method: "POST", path: "/v1/sessions", body: map[string]string{"email": sus.email, "password": sus.password}})
	if w.Code != 403 || problemCode(t, w) != "account_suspended" {
		t.Fatalf("suspended: %d %s", w.Code, w.Body)
	}
}

// 尚未绑定 TOTP 的管理员（AUTH-21）：第一步附 totp_enrollment，第二步只接受 TOTP 码，完成绑定并返回 10 个恢复码。
func TestConsoleLoginEnrollment(t *testing.T) {
	e := newEnv(t)
	a := e.account(t, false, "superadmin")
	w := e.do(req{method: "POST", path: "/v1/sessions", body: map[string]string{"email": a.email, "password": a.password}})
	if w.Code != 401 || problemCode(t, w) != "mfa_required" {
		t.Fatalf("step 1: %d %s", w.Code, w.Body)
	}
	p := problem(t, w)
	enroll, _ := p["totp_enrollment"].(map[string]any)
	if enroll == nil || !strings.HasPrefix(enroll["otpauth_uri"].(string), "otpauth://totp/") {
		t.Fatalf("totp_enrollment = %v", p["totp_enrollment"])
	}
	if m, _ := p["methods"].([]any); len(m) != 1 || m[0] != "totp" {
		t.Fatalf("methods = %v", p["methods"])
	}
	secret, err := b32decode(enroll["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	challenge := p["challenge_id"].(string)
	w = e.do(req{method: "POST", path: "/v1/sessions", body: map[string]string{"challenge_id": challenge, "recovery_code": "aaaaa-bbbbb"}})
	if f, c := fieldCode(t, w); w.Code != 400 || f != "recovery_code" || c != "not_allowed" {
		t.Fatalf("recovery code at enrollment: %d %s", w.Code, w.Body)
	}
	w = e.do(req{method: "POST", path: "/v1/sessions", body: map[string]string{"challenge_id": challenge, "totp_code": "000000"}})
	if w.Code != 401 || e.count(t, `SELECT count(*) FROM mfa_totp WHERE account_id = $1`, a.id) != 0 {
		t.Fatalf("wrong code: %d, totp rows %d", w.Code, e.count(t, `SELECT count(*) FROM mfa_totp WHERE account_id = $1`, a.id))
	}
	e.clk.Advance(mfa.Period)
	w = e.do(req{method: "POST", path: "/v1/sessions", body: map[string]string{"challenge_id": challenge, "totp_code": mfa.Code(secret, mfa.Step(e.clk.Now()))}})
	if w.Code != 201 {
		t.Fatalf("step 2: %d %s", w.Code, w.Body)
	}
	var out struct {
		RecoveryCodes []string `json:"recovery_codes"`
		Staff         struct {
			HasTotp bool `json:"has_totp"`
		} `json:"staff"`
	}
	decode(t, w, &out)
	if len(out.RecoveryCodes) != 10 || !out.Staff.HasTotp {
		t.Fatalf("response = %+v", out)
	}
	// 之后以恢复码登录（非首次）。
	a.secret = secret
	w = e.do(req{method: "POST", path: "/v1/sessions", body: map[string]string{"email": a.email, "password": a.password}})
	challenge = problem(t, w)["challenge_id"].(string)
	w = e.do(req{method: "POST", path: "/v1/sessions", body: map[string]string{"challenge_id": challenge, "recovery_code": out.RecoveryCodes[0]}})
	if w.Code != 201 {
		t.Fatalf("recovery code login: %d %s", w.Code, w.Body)
	}
}

// 管理会话（AUTH-21）：刷新轮换，空闲 30 分钟失效，绝对 12 小时失效；刷新后的令牌仍可访问管理接口（带 amr）。
func TestConsoleRefresh(t *testing.T) {
	e := newEnv(t)
	a := e.staff(t, "operator")
	w := e.refreshReq(a)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("refresh: %d %s", w.Code, w.Body)
	}
	var out struct {
		RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	}
	decode(t, w, &out)
	login := e.clk.Now()
	e.takeCookies(t, a, w)
	if w := e.do(req{method: "GET", path: "/v1/staff/me", as: a}); w.Code != 200 {
		t.Fatalf("after refresh: %d", w.Code)
	}
	if d := out.RefreshExpiresAt.Sub(login); d > session.ConsoleAbsolute || d < session.ConsoleAbsolute-time.Minute {
		t.Fatalf("refresh_expires_at is %v after login, want the 12h absolute expiry", d)
	}
	// 空闲 30 分钟失效。
	e.clk.Advance(session.ConsoleIdle + time.Second)
	if w := e.refreshReq(a); w.Code != 400 {
		t.Fatalf("idle refresh: %d %s", w.Code, w.Body)
	}
	// 每 29 分钟刷新一次，12 小时后绝对失效。
	b := e.staff(t, "operator")
	start := e.clk.Now()
	for e.clk.Now().Sub(start) < session.ConsoleAbsolute-30*time.Minute {
		e.clk.Advance(29 * time.Minute)
		w := e.refreshReq(b)
		if w.Code != 200 {
			t.Fatalf("refresh at %v: %d", e.clk.Now().Sub(start), w.Code)
		}
		e.takeCookies(t, b, w)
	}
	e.clk.Advance(session.ConsoleAbsolute - e.clk.Now().Sub(start) + time.Second)
	if w := e.refreshReq(b); w.Code != 400 {
		t.Fatalf("refresh after 12h: %d", w.Code)
	}
	if w := e.refreshReq(&admin{refresh: "not-a-token"}); w.Code != 400 {
		t.Fatalf("garbage refresh: %d", w.Code)
	}
	// 管理会话的刷新令牌不能在客户端接口使用（AUTH-21），且不因此吊销会话链。
	c := e.staff(t, "operator")
	if _, err := e.sessions.Refresh(t.Context(), c.refresh, "", ""); !errors.Is(err, session.ErrInvalidGrant) {
		t.Fatalf("client refresh with a console token: %v", err)
	}
	if w := e.refreshReq(c); w.Code != 200 {
		t.Fatalf("console refresh after the rejected client attempt: %d", w.Code)
	}
}

// 登出吊销会话链并写审计 session.delete；之后访问令牌与 Mfa-Assertion 都失效（AUTH-19，M1-02 验收 3）。
func TestConsoleLogout(t *testing.T) {
	e := newEnv(t)
	a := e.staff(t, "superadmin")
	e.stepUp(t, a)
	if w := e.do(req{method: "DELETE", path: "/v1/sessions/current", as: a}); w.Code != 204 {
		t.Fatalf("logout: %d %s", w.Code, w.Body)
	}
	if w := e.do(req{method: "GET", path: "/v1/staff/me", as: a}); w.Code != 401 {
		t.Fatalf("after logout: %d", w.Code)
	}
	assertion := a.assertion
	e.login(t, a) // 新的会话链
	a.assertion = assertion
	w := e.do(req{method: "POST", path: "/v1/roles", as: a, sensitive: true,
		body: map[string]any{"name": "viewer", "permissions": []string{"orders.read"}, "reason": "x"}})
	if w.Code != 401 || problemCode(t, w) != "mfa_required" {
		t.Fatalf("old assertion on a new session chain: %d %s", w.Code, w.Body)
	}
	if got := e.auditActions(t); !slices.Equal(got, []string{"session.create", "step_up.create", "session.delete", "session.create"}) {
		t.Fatalf("audit = %v", got)
	}
}

// step-up（AUTH-19）：只接受 TOTP；错误不写审计并返回 400 incorrect；Mfa-Assertion 在有效期内可复用，
// 刷新轮换后仍属于同一会话链；同一账号的其他会话链不能使用。
func TestStepUp(t *testing.T) {
	e := newEnv(t)
	a := e.staff(t, "superadmin")
	w := e.do(req{method: "POST", path: "/v1/staff/me/step-up", as: a, body: map[string]string{"totp_code": "000000"}})
	if f, c := fieldCode(t, w); w.Code != 400 || f != "totp_code" || c != "incorrect" {
		t.Fatalf("wrong code: %d %s", w.Code, w.Body)
	}
	w = e.do(req{method: "POST", path: "/v1/staff/me/step-up", as: a, body: map[string]string{"recovery_code": "aaaaa-bbbbb"}})
	if w.Code != 400 {
		t.Fatalf("recovery code: %d %s", w.Code, w.Body)
	}
	if got := e.auditActions(t); !slices.Equal(got, []string{"session.create"}) {
		t.Fatalf("audit after failures = %v", got)
	}
	e.stepUp(t, a)
	for _, name := range []string{"viewer-a", "viewer-b"} {
		w := e.do(req{method: "POST", path: "/v1/roles", as: a, sensitive: true,
			body: map[string]any{"name": name, "permissions": []string{"orders.read"}, "reason": "x"}})
		if w.Code != 201 {
			t.Fatalf("reuse %s: %d %s", name, w.Code, w.Body)
		}
	}
	// 刷新轮换后仍可使用。
	rw := e.refreshReq(a)
	e.takeCookies(t, a, rw)
	w = e.do(req{method: "POST", path: "/v1/roles", as: a, sensitive: true,
		body: map[string]any{"name": "viewer-c", "permissions": []string{"orders.read"}, "reason": "x"}})
	if w.Code != 201 {
		t.Fatalf("after refresh: %d %s", w.Code, w.Body)
	}
	// 同一账号的另一会话链不能使用。
	other := &admin{id: a.id, email: a.email, password: a.password, secret: a.secret}
	e.login(t, other)
	other.assertion = a.assertion
	w = e.do(req{method: "POST", path: "/v1/roles", as: other, sensitive: true,
		body: map[string]any{"name": "viewer-d", "permissions": []string{"orders.read"}, "reason": "x"}})
	if w.Code != 401 {
		t.Fatalf("other chain: %d %s", w.Code, w.Body)
	}
}

func b32decode(s string) ([]byte, error) {
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
}
