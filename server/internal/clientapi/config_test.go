// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/akari-project/panel/server/internal/clientconfig"
)

type signedConfig struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
	KeyID     string          `json:"key_id"`
}

func (e *env) getConfig(t *testing.T, ua, inm string) (int, http.Header, signedConfig, map[string]any) {
	t.Helper()
	w := e.get("/v1/config", func(r *http.Request) {
		if ua != "" {
			r.Header.Set("User-Agent", ua)
		}
		if inm != "" {
			r.Header.Set("If-None-Match", inm)
		}
	})
	var doc signedConfig
	var payload map[string]any
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("body: %v %s", err, w.Body)
		}
		_ = json.Unmarshal(doc.Payload, &payload)
	}
	return w.Code, w.Header(), doc, payload
}

func (e *env) setSetting(t *testing.T, key, value string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO settings (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value); err != nil {
		t.Fatal(err)
	}
}

// GET /v1/config（spec/30 API-11）：签名可用公钥按 payload 的 JCS 字节验证；默认值；ETag 与 304；Cache-Control。
func TestGetConfig(t *testing.T) {
	e := newEnv(t)
	code, h, doc, p := e.getConfig(t, "", "")
	if code != 200 || h.Get("Cache-Control") != "no-cache" || h.Get("ETag") == "" || h.Get("Content-Type") != "application/json" {
		t.Fatalf("get: %d %v", code, h)
	}
	if doc.KeyID != "5" {
		t.Errorf("key_id = %q", doc.KeyID)
	}
	// 客户端按收到的 payload 重新规范化后验签。
	canon, err := clientconfig.Canonical(p)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := base64.StdEncoding.DecodeString(doc.Signature)
	if !ed25519.Verify(e.signer.PublicKey(), canon, sig) || string(canon) != string(doc.Payload) {
		t.Fatalf("signature does not verify over %s", doc.Payload)
	}
	want := map[string]any{
		"min_version": map[string]any{}, "announcement_version": float64(0), "registration_policy": "open",
		"features":      map[string]any{"announcements": false, "articles": false, "support": false, "referrals": false, "diagnostics": false},
		"api_endpoints": []any{"https://api.example.com", "https://api-backup.example.net/panel"},
		"issued_at":     t0.UTC().Format(time.RFC3339),
	}
	if got, _ := json.Marshal(p); string(got) != mustJSON(want) {
		t.Fatalf("payload = %s\nwant      %s", got, mustJSON(want))
	}

	// 未变化：304；If-None-Match 为列表或弱比较时同样命中。
	etag := h.Get("ETag")
	for _, inm := range []string{etag, `"x", ` + etag, "W/" + etag, "*"} {
		if code, h, _, _ := e.getConfig(t, "", inm); code != 304 || h.Get("ETag") != etag {
			t.Errorf("If-None-Match %s: %d", inm, code)
		}
	}

	// issued_at 首次写入后固定：时钟前进后仍相同，ETag 不变（各副本一致）。
	e.clk.Advance(time.Hour)
	if _, h, _, p := e.getConfig(t, "", ""); h.Get("ETag") != etag || p["issued_at"] != want["issued_at"] {
		t.Fatalf("document changed without input change: %v", p["issued_at"])
	}

	// 输入变化：新文档、新 ETag；未知平台与格式错误的版本不下发。
	e.setSetting(t, "min_version", `{"ios":"1.4.0","web":"1.0.0","android":"01.0.0"}`)
	e.setSetting(t, "features", `{"support":true,"unknown":true}`)
	e.setSetting(t, "registration_policy", `"invite_only"`)
	_, h, _, p = e.getConfig(t, "", etag)
	if h.Get("ETag") == etag || mustJSON(p["min_version"]) != `{"ios":"1.4.0"}` || p["registration_policy"] != "invite_only" ||
		mustJSON(p["features"]) != `{"announcements":false,"articles":false,"diagnostics":false,"referrals":false,"support":true}` {
		t.Fatalf("after settings change: %v %v", h.Get("ETag"), p)
	}
}

// settings 中出现契约之外的注册策略时按 open 签发，不下发非法取值。
func TestConfigRegistrationPolicyFallback(t *testing.T) {
	e := newEnv(t)
	e.setSetting(t, "registration_policy", `"members_only"`)
	if _, _, _, p := e.getConfig(t, "", ""); p["registration_policy"] != "open" {
		t.Fatalf("registration_policy = %v", p["registration_policy"])
	}
}

// API-03：只有契约列出 426 的入口操作检查自研客户端最低版本；浏览器、第三方客户端与 /v1/config 不检查。
func TestUpgradeRequired(t *testing.T) {
	e := newEnv(t)
	e.register(t, "old@example.com", "correct horse battery")
	e.setSetting(t, "min_version", `{"ios":"1.4.0"}`)
	login := jsonBody(map[string]any{"email": "old@example.com", "password": "correct horse battery", "device": webDevice})
	for _, tc := range []struct {
		ua   string
		want int
	}{
		{"Akari/1.3.9 (iOS 19.1)", 426},
		{"Akari/1.3.9 (iPadOS 19.1)", 426},
		{"Akari/1.4.0 (iOS 19.1)", 201},
		{"Akari/1.0.0 (Android 16)", 201},                 // 未配置的平台
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 19_1)", 201}, // 浏览器
		{"clash-verge/1.0.0 (iOS 19.1)", 201},             // 第三方客户端
	} {
		w := e.do(req{method: "POST", path: "/v1/sessions", body: login, ua: tc.ua})
		if w.Code != tc.want || (tc.want == 426 && problemCode(t, w) != "upgrade_required") {
			t.Errorf("%s: %d %s", tc.ua, w.Code, w.Body)
		}
	}
	if w := e.do(req{method: "POST", path: "/v1/sessions/nonces", ua: "Akari/1.0.0 (iOS 19.1)"}); w.Code != 426 {
		t.Errorf("session nonce: %d", w.Code)
	}
	if code, _, _, _ := e.getConfig(t, "Akari/1.0.0 (iOS 19.1)", ""); code != 200 {
		t.Errorf("config with outdated client: %d", code)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
