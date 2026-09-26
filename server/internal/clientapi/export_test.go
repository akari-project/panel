// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/account"
	"github.com/akari-project/panel/server/internal/session"
)

func exportToken(t *testing.T, u string) string {
	t.Helper()
	const prefix = "https://api.example.com/v1/configurations/"
	if !strings.HasPrefix(u, prefix) || len(u) != len(prefix)+43 {
		t.Fatalf("export url %q", u)
	}
	return strings.TrimPrefix(u, prefix)
}

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type exportLink struct {
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
}

func (e *env) exportLink(t *testing.T, bearer string) exportLink {
	t.Helper()
	w := e.get("/v1/me/export-link", bearerAuth(bearer))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("export link: %d %s %v", w.Code, w.Body, w.Header())
	}
	var l exportLink
	_ = json.Unmarshal(w.Body.Bytes(), &l)
	return l
}

// AUTH-16：导出令牌在注册时生成，链接稳定且不缓存；重置需要重新验证，未通过时不写库；
// 重置在同一事务中换新令牌（旧哈希失效）并轮换共用凭据（revoked、rotated）。
func TestExportLink(t *testing.T) {
	e := newEnv(t)
	e.register(t, "exp@example.com", "correct horse battery")
	var acct uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM accounts WHERE email = 'exp@example.com'`).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM export_tokens WHERE account_id = $1 AND rotated_at = $2`, acct, t0); n != 1 {
		t.Fatal("export token not created with the account")
	}
	s := e.appLogin(t, "exp@example.com", "correct horse battery")
	first := e.exportLink(t, s.AccessToken)
	tok := exportToken(t, first.URL)
	if again := e.exportLink(t, s.AccessToken); again.URL != first.URL || !again.CreatedAt.Equal(t0) || !first.CreatedAt.Equal(t0) {
		t.Fatalf("export link not stable: %+v then %+v", first, again)
	}
	if n := e.count(t, `SELECT count(*) FROM export_tokens WHERE token_hash = $1`, hashOf(tok)); n != 1 {
		t.Fatal("token_hash is not the SHA-256 of the token")
	}

	// 登录的重新验证状态过期后，重置返回 401 mfa_required，且不写库（令牌、共用凭据、事件都不变）。
	e.clk.Advance(session.ReauthTTL + time.Second)
	events := len(e.credentialEvents(t, "exp@example.com"))
	w := e.do(req{method: "POST", path: "/v1/me/export-link/rotation", bearer: s.AccessToken})
	if w.Code != 401 || problemCode(t, w) != "mfa_required" {
		t.Fatalf("rotate without reauth: %d %s", w.Code, w.Body)
	}
	if e.count(t, `SELECT count(*) FROM export_tokens WHERE token_hash = $1 AND rotated_at = $2`, hashOf(tok), t0) != 1 ||
		len(e.credentialEvents(t, "exp@example.com")) != events ||
		e.count(t, `SELECT count(*) FROM proxy_credentials WHERE account_id = $1 AND device_id IS NULL`, acct) != 1 {
		t.Fatal("rejected rotation wrote to the database")
	}

	var shared uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM proxy_credentials WHERE account_id = $1 AND device_id IS NULL`, acct).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	if w := e.do(req{method: "POST", path: "/v1/me/reauthentications", bearer: s.AccessToken, body: `{"password":"correct horse battery"}`}); w.Code != 200 {
		t.Fatalf("reauth: %d %s", w.Code, w.Body)
	}
	now := e.clk.Now()
	// 响应含秘密值，不接受 Idempotency-Key（CONV-12）：请求头被忽略，不保存幂等记录。
	w = e.do(req{method: "POST", path: "/v1/me/export-link/rotation", bearer: s.AccessToken, idempotencyKey: uuid.NewString()})
	if n := e.count(t, `SELECT count(*) FROM idempotency_keys`); n != 0 {
		t.Fatalf("%d idempotency records saved for a secret-bearing response", n)
	}
	var rotated exportLink
	_ = json.Unmarshal(w.Body.Bytes(), &rotated)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "private, no-store" || !rotated.CreatedAt.Equal(now) {
		t.Fatalf("rotate: %d %s %v", w.Code, w.Body, w.Header())
	}
	newTok := exportToken(t, rotated.URL)
	if newTok == tok || e.count(t, `SELECT count(*) FROM export_tokens WHERE token_hash = $1`, hashOf(tok)) != 0 ||
		e.count(t, `SELECT count(*) FROM export_tokens WHERE token_hash = $1`, hashOf(newTok)) != 1 {
		t.Fatal("old token still valid after rotation")
	}
	if got := e.exportLink(t, s.AccessToken); got.URL != rotated.URL || !got.CreatedAt.Equal(rotated.CreatedAt) {
		t.Fatalf("GET after rotation = %+v, want %+v", got, rotated)
	}
	if ev := e.credentialEvents(t, "exp@example.com")[events:]; !slices.Equal(ev, []string{"revoked", "rotated"}) {
		t.Fatalf("rotation events = %v", ev)
	}
	if e.count(t, `SELECT count(*) FROM proxy_credentials WHERE id = $1 AND revoked_at IS NOT NULL`, shared) != 1 ||
		e.count(t, `SELECT count(*) FROM proxy_credentials WHERE account_id = $1 AND device_id IS NULL AND revoked_at IS NULL`, acct) != 1 {
		t.Fatal("shared credential not rotated")
	}
	var enc []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT secret_enc FROM proxy_credentials WHERE account_id = $1 AND device_id IS NULL AND revoked_at IS NULL`, acct).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if pt, err := e.keys.Open(enc, account.CredentialSecretAD); err != nil || len(pt) != 16 {
		t.Fatalf("rotated shared secret is %d bytes (%v), want 16 raw bytes (AGT-15)", len(pt), err)
	}

	// AUTH-22 的凭据重置删除导出令牌；读取时补建新令牌。
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM export_tokens WHERE account_id = $1`, acct); err != nil {
		t.Fatal(err)
	}
	later := e.clk.Advance(time.Minute)
	recreated := e.exportLink(t, s.AccessToken)
	if exportToken(t, recreated.URL) == newTok || !recreated.CreatedAt.Equal(later) {
		t.Fatalf("recreated link = %+v", recreated)
	}
}
