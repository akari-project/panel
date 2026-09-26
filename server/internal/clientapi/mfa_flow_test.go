// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/mfa"
	"github.com/akari-project/panel/server/internal/session"
)

// enableTOTP 为账号绑定并启用 TOTP，返回密钥与恢复码。
func (e *env) enableTOTP(t *testing.T, access string) ([]byte, []string) {
	t.Helper()
	w := e.do(req{method: "POST", path: "/v1/me/mfa/totp", bearer: access})
	var en struct {
		Secret     string `json:"secret"`
		OtpauthURI string `json:"otpauth_uri"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &en)
	if w.Code != 201 || !strings.HasPrefix(en.OtpauthURI, "otpauth://totp/") {
		t.Fatalf("start enrollment: %d %s", w.Code, w.Body)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(en.Secret)
	if err != nil {
		t.Fatal(err)
	}
	w = e.do(req{method: "POST", path: "/v1/me/mfa/totp/activation", bearer: access, body: `{"totp_code":"000000"}`})
	if f, c := fieldCode(t, w); c != "incorrect" && mfa.Code(secret, mfa.Step(e.clk.Now())) != "000000" {
		t.Fatalf("wrong activation code: %s %s", f, c)
	}
	w = e.do(req{method: "POST", path: "/v1/me/mfa/totp/activation", bearer: access,
		body: jsonBody(map[string]string{"totp_code": mfa.Code(secret, mfa.Step(e.clk.Now()))})})
	var rc struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &rc)
	if w.Code != 200 || len(rc.RecoveryCodes) != 10 {
		t.Fatalf("activate: %d %s", w.Code, w.Body)
	}
	e.clk.Advance(mfa.Period) // 下一次使用必须晚于已使用的时间步（AUTH-11）
	return secret, rc.RecoveryCodes
}

type mfaProblem struct {
	Code        string   `json:"code"`
	ChallengeID string   `json:"challenge_id"`
	Methods     []string `json:"methods"`
}

func decodeMFA(w *httptest.ResponseRecorder) mfaProblem {
	var p mfaProblem
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	return p
}

func (e *env) secondStep(t *testing.T, challenge string, device map[string]any, field, code string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(post("/v1/sessions", jsonBody(map[string]any{"challenge_id": challenge, "device": device, field: code})))
}

// AUTH-11、AUTH-20：启用 TOTP 后登录需要第二步；同一时间步的码不能再次使用；恢复码只能使用一次。
func TestTOTPLogin(t *testing.T) {
	e := newEnv(t)
	e.register(t, "totp@example.com", "correct horse battery")
	s := e.appLogin(t, "totp@example.com", "correct horse battery")
	secret, codes := e.enableTOTP(t, s.AccessToken)
	if w := e.do(req{method: "POST", path: "/v1/me/mfa/totp", bearer: s.AccessToken}); w.Code != 409 {
		t.Fatalf("enroll while enabled: %d", w.Code)
	}
	if w := e.get("/v1/me", bearerAuth(s.AccessToken)); !strings.Contains(w.Body.String(), `"is_mfa_enabled":true`) {
		t.Fatalf("me: %s", w.Body)
	}

	w := e.login(t, "totp@example.com", "correct horse battery", webDevice)
	p := decodeMFA(w)
	if w.Code != 401 || p.Code != "mfa_required" || p.ChallengeID == "" || strings.Join(p.Methods, ",") != "totp,recovery_code" {
		t.Fatalf("step 1: %d %s", w.Code, w.Body)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("cookies issued before second step")
	}
	// 设备与第一步不同：拒绝。
	d, _ := appDevice(t)
	code := mfa.Code(secret, mfa.Step(e.clk.Now()))
	if w := e.secondStep(t, p.ChallengeID, d, "totp_code", code); w.Code != 401 {
		t.Fatalf("different device: %d", w.Code)
	}
	if w := e.secondStep(t, p.ChallengeID, webDevice, "totp_code", code); w.Code != 201 || len(w.Result().Cookies()) != 2 {
		t.Fatalf("step 2: %d %s", w.Code, w.Body)
	}
	// 挑战用后作废；同一时间步的码不能再次使用。
	if w := e.secondStep(t, p.ChallengeID, webDevice, "totp_code", code); w.Code != 401 {
		t.Fatalf("challenge reused: %d", w.Code)
	}
	p2 := decodeMFA(e.login(t, "totp@example.com", "correct horse battery", webDevice))
	if w := e.secondStep(t, p2.ChallengeID, webDevice, "totp_code", code); w.Code != 401 {
		t.Fatalf("totp code reused within the same step: %d", w.Code)
	}
	// 恢复码（不区分大小写与分隔符）只能使用一次。
	if w := e.secondStep(t, p2.ChallengeID, webDevice, "recovery_code", strings.ToUpper(strings.ReplaceAll(codes[0], "-", ""))); w.Code != 201 {
		t.Fatalf("recovery code: %d %s", w.Code, w.Body)
	}
	p3 := decodeMFA(e.login(t, "totp@example.com", "correct horse battery", webDevice))
	if w := e.secondStep(t, p3.ChallengeID, webDevice, "recovery_code", codes[0]); w.Code != 401 {
		t.Fatalf("recovery code reused: %d", w.Code)
	}
}

// AUTH-20：每个挑战最多尝试 5 次，用完即作废。
func TestChallengeAttempts(t *testing.T) {
	e := newEnv(t)
	e.register(t, "attempts@example.com", "correct horse battery")
	s := e.appLogin(t, "attempts@example.com", "correct horse battery")
	secret, _ := e.enableTOTP(t, s.AccessToken)
	p := decodeMFA(e.login(t, "attempts@example.com", "correct horse battery", webDevice))
	good := mfa.Code(secret, mfa.Step(e.clk.Now()))
	bad := "000000"
	if good == bad {
		bad = "111111"
	}
	for i := range 4 {
		if w := e.secondStep(t, p.ChallengeID, webDevice, "totp_code", bad); w.Code != 401 {
			t.Fatalf("attempt %d: %d", i+1, w.Code)
		}
	}
	// 密码正确的第一步不清除失败计数，二次验证失败同样计入（AUTH-09）：第一步加 4 次失败共 5 次，
	// 之后的尝试进入冷却。
	if w := e.secondStep(t, p.ChallengeID, webDevice, "totp_code", bad); w.Code != 429 {
		t.Fatalf("attempt 5: %d", w.Code)
	}
	if w := e.secondStep(t, p.ChallengeID, webDevice, "totp_code", good); w.Code == 201 {
		t.Fatal("challenge usable after 5 failures")
	}
}

// AUTH-23：停用二次验证、重新生成恢复码、修改密码需要 5 分钟内重新验证。
func TestStepUp(t *testing.T) {
	e := newEnv(t)
	e.register(t, "stepup@example.com", "correct horse battery")
	s := e.appLogin(t, "stepup@example.com", "correct horse battery")
	other := e.appLogin(t, "stepup@example.com", "correct horse battery")
	secret, _ := e.enableTOTP(t, s.AccessToken)
	e.clk.Advance(session.ReauthTTL) // 登录时的重新验证窗口过期（AUTH-23）

	w := e.do(req{method: "DELETE", path: "/v1/me/mfa/totp", bearer: s.AccessToken})
	if p := decodeMFA(w); w.Code != 401 || p.Code != "mfa_required" || strings.Join(p.Methods, ",") != "totp,recovery_code" {
		t.Fatalf("without step-up: %d %s", w.Code, w.Body)
	}
	w = e.do(req{method: "POST", path: "/v1/me/reauthentications", bearer: s.AccessToken, body: `{"password":"wrong password"}`})
	if f, c := fieldCode(t, w); w.Code != 400 || f != "password" || c != "incorrect" {
		t.Fatalf("wrong password: %d %s", w.Code, w.Body)
	}
	w = e.do(req{method: "POST", path: "/v1/me/reauthentications", bearer: s.AccessToken,
		body: jsonBody(map[string]string{"totp_code": mfa.Code(secret, mfa.Step(e.clk.Now()))})})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "expires_at") {
		t.Fatalf("reauth with totp: %d %s", w.Code, w.Body)
	}
	// 重新验证绑定会话：另一个会话仍需重新验证。
	if w := e.do(req{method: "POST", path: "/v1/me/mfa/recovery-codes", bearer: other.AccessToken}); w.Code != 401 {
		t.Fatalf("other session: %d", w.Code)
	}
	if w := e.do(req{method: "POST", path: "/v1/me/mfa/recovery-codes", bearer: s.AccessToken}); w.Code != 200 {
		t.Fatalf("regenerate: %d %s", w.Code, w.Body)
	}

	// 修改密码：吊销除当前会话外的全部会话，并发送安全通知。
	w = e.do(req{method: "PUT", path: "/v1/me/password", bearer: s.AccessToken, body: `{"new_password":"a new strong password"}`})
	if w.Code != 204 {
		t.Fatalf("change password: %d %s", w.Code, w.Body)
	}
	if w := e.get("/v1/me", bearerAuth(other.AccessToken)); w.Code != 401 {
		t.Fatalf("other session after password change: %d", w.Code)
	}
	if w := e.get("/v1/me", bearerAuth(s.AccessToken)); w.Code != 200 {
		t.Fatalf("current session after password change: %d", w.Code)
	}
	if n := e.count(t, `SELECT count(*) FROM notification_outbox WHERE template IN ('password_changed', 'mfa_enabled')`); n != 2 {
		t.Fatalf("security notifications = %d", n)
	}

	// 5 分钟后重新验证失效。
	e.clk.Advance(session.ReauthTTL)
	if w := e.do(req{method: "DELETE", path: "/v1/me/mfa/totp", bearer: s.AccessToken}); w.Code != 401 {
		t.Fatalf("after step-up expired: %d", w.Code)
	}
	_ = e.do(req{method: "POST", path: "/v1/me/reauthentications", bearer: s.AccessToken, body: `{"password":"a new strong password"}`})
	// 管理员不能停用二次验证（AUTH-12）。
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO roles (name, permissions, is_builtin) VALUES ('superadmin', '{*}', true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO account_roles (account_id, role) SELECT id, 'superadmin' FROM accounts WHERE email = 'stepup@example.com'`); err != nil {
		t.Fatal(err)
	}
	if w := e.do(req{method: "DELETE", path: "/v1/me/mfa/totp", bearer: s.AccessToken}); w.Code != 409 {
		t.Fatalf("staff disable: %d %s", w.Code, w.Body)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM account_roles`); err != nil {
		t.Fatal(err)
	}
	if w := e.do(req{method: "DELETE", path: "/v1/me/mfa/totp", bearer: s.AccessToken}); w.Code != 204 {
		t.Fatalf("disable: %d %s", w.Code, w.Body)
	}
	if w := e.login(t, "stepup@example.com", "a new strong password", webDevice); w.Code != 201 {
		t.Fatalf("login after disabling totp: %d", w.Code)
	}
}

// 以密码完成的登录视为一次重新验证，5 分钟内有效；刷新不延长（AUTH-23）。
func TestLoginCountsAsReauth(t *testing.T) {
	e := newEnv(t)
	e.register(t, "fresh@example.com", "correct horse battery")
	s := e.appLogin(t, "fresh@example.com", "correct horse battery")
	if w := e.do(req{method: "PUT", path: "/v1/me/password", bearer: s.AccessToken, body: `{"new_password":"a new strong password"}`}); w.Code != 204 {
		t.Fatalf("right after login: %d %s", w.Code, w.Body)
	}
	e.clk.Advance(session.ReauthTTL - time.Minute)
	_, pair := e.refresh(t, s.RefreshToken, "")
	access := pair["access_token"].(string)
	e.clk.Advance(time.Minute)
	if w := e.do(req{method: "PUT", path: "/v1/me/password", bearer: access, body: `{"new_password":"another strong password"}`}); w.Code != 401 {
		t.Fatalf("window extended: %d", w.Code)
	}
}

// 开始绑定 TOTP 需要重新验证（AUTH-23）；待确认密钥绑定会话链，其他会话不能确认，
// 刷新轮换后的会话可以确认（AUTH-11）。
func TestTOTPEnrollmentBinding(t *testing.T) {
	e := newEnv(t)
	e.register(t, "bind@example.com", "correct horse battery")
	s := e.appLogin(t, "bind@example.com", "correct horse battery")
	other := e.appLogin(t, "bind@example.com", "correct horse battery")
	e.clk.Advance(session.ReauthTTL)

	w := e.do(req{method: "POST", path: "/v1/me/mfa/totp", bearer: s.AccessToken})
	if p := decodeMFA(w); w.Code != 401 || p.Code != "mfa_required" || p.ChallengeID != "" {
		t.Fatalf("start without step-up: %d %s", w.Code, w.Body)
	}
	if w := e.do(req{method: "POST", path: "/v1/me/reauthentications", bearer: s.AccessToken, body: `{"password":"correct horse battery"}`}); w.Code != 200 {
		t.Fatalf("reauth: %d %s", w.Code, w.Body)
	}
	w = e.do(req{method: "POST", path: "/v1/me/mfa/totp", bearer: s.AccessToken})
	var en struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &en)
	if w.Code != 201 {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(en.Secret)
	if err != nil {
		t.Fatal(err)
	}
	code := func() string {
		return jsonBody(map[string]string{"totp_code": mfa.Code(secret, mfa.Step(e.clk.Now()))})
	}

	// 另一个会话（同一账号）不能确认。
	if w := e.do(req{method: "POST", path: "/v1/me/mfa/totp/activation", bearer: other.AccessToken, body: code()}); w.Code != 409 || problemCode(t, w) != "invalid_state" {
		t.Fatalf("other session activation: %d %s", w.Code, w.Body)
	}
	// 刷新轮换两次后，同一会话链中的会话可以确认。
	_, pair := e.refresh(t, s.RefreshToken, "")
	_, pair = e.refresh(t, pair["refresh_token"].(string), "")
	if w := e.do(req{method: "POST", path: "/v1/me/mfa/totp/activation", bearer: pair["access_token"].(string), body: code()}); w.Code != 200 {
		t.Fatalf("activation after refresh: %d %s", w.Code, w.Body)
	}
}

// 没有密码的账号（如管理员邀请、Passkey 账号）不能用任意密码完成重新验证（AUTH-23）。
func TestReauthWithoutPassword(t *testing.T) {
	e := newEnv(t)
	e.register(t, "nopw@example.com", "correct horse battery")
	s := e.appLogin(t, "nopw@example.com", "correct horse battery")
	e.clk.Advance(session.ReauthTTL) // 登录时的重新验证窗口过期
	if _, err := e.pool.Exec(context.Background(), `UPDATE accounts SET password_hash = NULL WHERE email = 'nopw@example.com'`); err != nil {
		t.Fatal(err)
	}
	w := e.do(req{method: "POST", path: "/v1/me/reauthentications", bearer: s.AccessToken, body: `{"password":"anything at all"}`})
	if f, c := fieldCode(t, w); w.Code != 400 || f != "password" || c != "incorrect" {
		t.Fatalf("reauth without password hash: %d %s", w.Code, w.Body)
	}
	if w := e.do(req{method: "PUT", path: "/v1/me/password", bearer: s.AccessToken, body: `{"new_password":"a new strong password"}`}); w.Code != 401 {
		t.Fatalf("step-up granted: %d", w.Code)
	}
}

// 重新验证的 5 分钟窗口按会话链计算：刷新轮换出的新会话继承剩余时间，不延长（AUTH-23）。
func TestReauthSurvivesRefresh(t *testing.T) {
	e := newEnv(t)
	e.register(t, "carry@example.com", "correct horse battery")
	s := e.appLogin(t, "carry@example.com", "correct horse battery")
	if w := e.do(req{method: "POST", path: "/v1/me/reauthentications", bearer: s.AccessToken, body: `{"password":"correct horse battery"}`}); w.Code != 200 {
		t.Fatalf("reauth: %d %s", w.Code, w.Body)
	}
	e.clk.Advance(2 * time.Minute)
	w, pair := e.refresh(t, s.RefreshToken, "")
	if w.Code != 200 {
		t.Fatalf("refresh: %d %s", w.Code, w.Body)
	}
	access := pair["access_token"].(string)
	if w := e.do(req{method: "PUT", path: "/v1/me/password", bearer: access, body: `{"new_password":"a new strong password"}`}); w.Code != 204 {
		t.Fatalf("step-up after refresh: %d %s", w.Code, w.Body)
	}
	e.clk.Advance(session.ReauthTTL - 2*time.Minute)
	if w := e.do(req{method: "PUT", path: "/v1/me/password", bearer: access, body: `{"new_password":"another strong password"}`}); w.Code != 401 {
		t.Fatalf("window extended by refresh: %d", w.Code)
	}
}

// AUTH-11：恢复码剩余不足 3 个时发送安全通知。
func TestRecoveryCodesLow(t *testing.T) {
	e := newEnv(t)
	e.register(t, "low@example.com", "correct horse battery")
	s := e.appLogin(t, "low@example.com", "correct horse battery")
	s2 := e.appLogin(t, "low@example.com", "correct horse battery")
	_, codes := e.enableTOTP(t, s.AccessToken)
	for i := range 8 {
		// 每个会话 15 分钟内最多 5 次重新验证，分两个会话进行。
		tok := s.AccessToken
		if i >= 4 {
			tok = s2.AccessToken
		}
		w := e.do(req{method: "POST", path: "/v1/me/reauthentications", bearer: tok, body: jsonBody(map[string]string{"recovery_code": codes[i]})})
		if w.Code != 200 {
			t.Fatalf("code %d: %d %s", i, w.Code, w.Body)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM notification_outbox WHERE template = 'recovery_codes_low' AND variables->>'remaining' = '2'`); n != 1 {
		t.Fatalf("low notifications = %d", n)
	}
}

// AUTH-09：他人输错密码使账号进入登录冷却时，已登录的会话仍可重新验证并修改密码。
func TestReauthNotBlockedByLoginCooldown(t *testing.T) {
	e := newEnv(t)
	e.register(t, "victim@example.com", "correct horse battery")
	s := e.appLogin(t, "victim@example.com", "correct horse battery")
	for range 6 {
		_ = e.login(t, "victim@example.com", "attacker guess!", webDevice)
	}
	if w := e.login(t, "victim@example.com", "correct horse battery", webDevice); w.Code != 429 {
		t.Fatalf("login during cooldown: %d", w.Code)
	}
	w := e.do(req{method: "POST", path: "/v1/me/reauthentications", bearer: s.AccessToken, body: `{"password":"correct horse battery"}`})
	if w.Code != 200 {
		t.Fatalf("reauth during cooldown: %d %s", w.Code, w.Body)
	}
	if w := e.do(req{method: "PUT", path: "/v1/me/password", bearer: s.AccessToken, body: `{"new_password":"a brand new password"}`}); w.Code != 204 {
		t.Fatalf("change password during cooldown: %d %s", w.Code, w.Body)
	}
}

// AUTH-09：并发的错误密码尝试在冷却生效前也不能超过 5 次校验。
func TestCooldownUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	e.register(t, "race@example.com", "correct horse battery")
	e.verifyCalls.Store(0)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 各请求来自不同 IP，排除按 IP 的限流。
			u := uuid.New()
			_ = e.do(req{method: "POST", path: "/v1/sessions", ip: "10.9." + strconv.Itoa(int(u[0])) + "." + strconv.Itoa(i),
				body: jsonBody(map[string]any{"email": "race@example.com", "password": "wrong password!", "device": webDevice})})
		}()
	}
	wg.Wait()
	if n := e.verifyCalls.Load(); n > 5 {
		t.Fatalf("%d password checks for one account", n)
	}
}

// AUTH-14：并发登录的多台设备不能超出设备上限（-race 下运行）。
func TestDeviceLimitUnderConcurrency(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			e := newEnv(t)
			// 并发登录不超过账号的失败预占上限（AUTH-09 MaxFailures），否则第 6 个起进入冷却。
			const logins = session.MaxFailures
			email := "limit" + strconv.Itoa(limit) + "@example.com"
			e.register(t, email, "correct horse battery")
			ctx := context.Background()
			if _, err := e.pool.Exec(ctx, `
				WITH p AS (INSERT INTO plans (name, tier, bytes_per_cycle, device_limit) VALUES ('p', 1, 0, $2) RETURNING id)
				INSERT INTO entitlements (account_id, plan_id, status, starts_at, cycle_start, bytes_limit, device_limit, reset_policy)
				SELECT a.id, p.id, 'active', $1, $1, 0, $2, 'never' FROM accounts a, p WHERE a.email = $3`, t0, limit, email); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			statuses := make(chan string, logins)
			for i := range logins {
				wg.Add(1)
				go func() {
					defer wg.Done()
					d, _ := appDevice(t)
					w := e.do(req{method: "POST", path: "/v1/sessions", ip: "10.8.0." + strconv.Itoa(i+1),
						body: jsonBody(map[string]any{"email": email, "password": "correct horse battery", "device": d})})
					var s sessionBody
					_ = json.Unmarshal(w.Body.Bytes(), &s)
					if w.Code != 201 {
						t.Errorf("login: %d %s", w.Code, w.Body)
					}
					statuses <- s.CredentialStatus
				}()
			}
			wg.Wait()
			close(statuses)
			issued, waiting := 0, 0
			for st := range statuses {
				switch st {
				case "issued":
					issued++
				case "device_limit_reached":
					waiting++
				}
			}
			if issued != limit || waiting != logins-limit ||
				e.count(t, `SELECT count(*) FROM proxy_credentials WHERE device_id IS NOT NULL AND revoked_at IS NULL`) != limit {
				t.Fatalf("issued %d credentials (%d waiting) for a limit of %d", issued, waiting, limit)
			}
		})
	}
}

// ADR 0017：同一刷新令牌的并发刷新都得到同一新令牌对，不判为泄露。
func TestConcurrentRefresh(t *testing.T) {
	e := newEnv(t)
	e.register(t, "multi@example.com", "correct horse battery")
	s := e.appLogin(t, "multi@example.com", "correct horse battery")
	var wg sync.WaitGroup
	results := make(chan string, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, m := e.refresh(t, s.RefreshToken, "")
			if w.Code != 200 {
				results <- "error " + w.Body.String()
				return
			}
			results <- m["refresh_token"].(string)
		}()
	}
	wg.Wait()
	close(results)
	var first string
	for r := range results {
		if first == "" {
			first = r
		}
		if r != first || strings.HasPrefix(r, "error") {
			t.Fatalf("concurrent refresh results differ: %q vs %q", r, first)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM sessions WHERE revoked_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d sessions revoked by concurrent refresh", n)
	}
}
