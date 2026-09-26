// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/httpx"
)

// entitle 为账号设置 active 权益，快照 device_limit 为 limit。
func (e *env) entitle(t *testing.T, email string, limit int) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `
		WITH p AS (INSERT INTO plans (name, tier, bytes_per_cycle, device_limit) VALUES ($3, 1, 0, $2) RETURNING id)
		INSERT INTO entitlements (account_id, plan_id, status, starts_at, cycle_start, bytes_limit, device_limit, reset_policy)
		SELECT a.id, p.id, 'active', $1, $1, 0, $2, 'never' FROM accounts a, p WHERE a.email = $4`, t0, limit, uuid.NewString(), email); err != nil {
		t.Fatal(err)
	}
}

type deviceItem struct {
	ID            uuid.UUID  `json:"id"`
	Platform      string     `json:"platform"`
	Model         *string    `json:"model"`
	AppVersion    *string    `json:"app_version"`
	CreatedAt     time.Time  `json:"created_at"`
	LastSeenAt    *time.Time `json:"last_seen_at"`
	IPPrefix      *string    `json:"ip_prefix"`
	IsCurrent     bool       `json:"is_current"`
	HasCredential bool       `json:"has_credential"`
}

type deviceList struct {
	DeviceLimit int          `json:"device_limit"`
	Items       []deviceItem `json:"items"`
}

func (e *env) devices(t *testing.T, bearer string) deviceList {
	t.Helper()
	w := e.get("/v1/me/devices", bearerAuth(bearer))
	if w.Code != 200 {
		t.Fatalf("list devices: %d %s", w.Code, w.Body)
	}
	for _, f := range []string{`"model":`, `"app_version":`, `"last_seen_at":`, `"ip_prefix":`} {
		if !strings.Contains(w.Body.String(), f) {
			t.Fatalf("nullable field %s omitted: %s", f, w.Body)
		}
	}
	var l deviceList
	if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l deviceList) get(id uuid.UUID) deviceItem {
	for _, d := range l.Items {
		if d.ID == id {
			return d
		}
	}
	return deviceItem{}
}

func (e *env) removeDevice(bearer string, id uuid.UUID) int {
	return e.do(req{method: "DELETE", path: "/v1/me/devices/" + id.String(), bearer: bearer}).Code
}

// credentialEvents 返回某账号 credential.changed 事件的 change 取值（按写入顺序）。
func (e *env) credentialEvents(t *testing.T, email string) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT o.payload->>'change' FROM outbox o JOIN accounts a ON a.id = (o.payload->>'account_id')::uuid
		WHERE o.topic = 'credential.changed' AND a.email = $1 ORDER BY o.id`, email)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// M1-03 验收：上限为 1 时第一台设备取得凭据，第二台 device_limit_reached；移除第一台后第二台自动取得凭据，
// 第三台等待；移除写 credential.changed（AUTH-14、AUTH-15）。
func TestDeviceLimitAndRemoval(t *testing.T) {
	e := newEnv(t)
	e.register(t, "dev@example.com", "correct horse battery")
	e.entitle(t, "dev@example.com", 1)
	a := e.appLogin(t, "dev@example.com", "correct horse battery")
	b := e.appLogin(t, "dev@example.com", "correct horse battery")
	if a.CredentialStatus != "issued" || b.CredentialStatus != "device_limit_reached" {
		t.Fatalf("statuses = %s, %s", a.CredentialStatus, b.CredentialStatus)
	}
	l := e.devices(t, b.AccessToken)
	if l.DeviceLimit != 1 || len(l.Items) != 2 || !l.get(a.DeviceID).HasCredential || l.get(b.DeviceID).HasCredential ||
		!l.get(b.DeviceID).IsCurrent || l.get(a.DeviceID).IsCurrent {
		t.Fatalf("devices = %+v", l)
	}
	before := e.credentialEvents(t, "dev@example.com") // 共用凭据 created、A created
	if !slices.Equal(before, []string{"created", "created"}) {
		t.Fatalf("events before removal = %v", before)
	}

	if code := e.removeDevice(b.AccessToken, a.DeviceID); code != 204 {
		t.Fatalf("remove A: %d", code)
	}
	l = e.devices(t, b.AccessToken)
	if len(l.Items) != 1 || l.Items[0].ID != b.DeviceID || !l.Items[0].HasCredential {
		t.Fatalf("after removal = %+v", l)
	}
	if ev := e.credentialEvents(t, "dev@example.com"); !slices.Equal(ev, []string{"created", "created", "revoked", "created"}) {
		t.Fatalf("events after removal = %v", ev)
	}
	// A 的会话随设备吊销：访问令牌立即失效，刷新令牌无效。
	if w := e.get("/v1/me", bearerAuth(a.AccessToken)); w.Code != 401 {
		t.Fatalf("removed device access: %d", w.Code)
	}
	if w, m := e.refresh(t, a.RefreshToken, ""); w.Code != 400 || m["error"] != "invalid_grant" {
		t.Fatalf("removed device refresh: %d %s", w.Code, w.Body)
	}
	c := e.appLogin(t, "dev@example.com", "correct horse battery")
	if c.CredentialStatus != "device_limit_reached" {
		t.Fatalf("third device = %s", c.CredentialStatus)
	}
	// 已移除的设备、不存在的设备与其他账号的设备都返回 404（CONV-15），不产生事件。
	e.register(t, "other@example.com", "correct horse battery")
	o := e.appLogin(t, "other@example.com", "correct horse battery")
	n := len(e.credentialEvents(t, "dev@example.com"))
	for _, id := range []uuid.UUID{a.DeviceID, uuid.New(), o.DeviceID} {
		if code := e.removeDevice(b.AccessToken, id); code != 404 {
			t.Errorf("remove %s: %d, want 404", id, code)
		}
	}
	if e.count(t, `SELECT count(*) FROM devices WHERE id = $1 AND revoked_at IS NULL`, o.DeviceID) != 1 ||
		len(e.credentialEvents(t, "dev@example.com")) != n {
		t.Fatal("404 removal changed state")
	}
}

// AUTH-10：登出释放名额，等待中的设备随即取得凭据。
func TestLogoutReleasesSlot(t *testing.T) {
	e := newEnv(t)
	e.register(t, "out@example.com", "correct horse battery")
	e.entitle(t, "out@example.com", 1)
	a := e.appLogin(t, "out@example.com", "correct horse battery")
	b := e.appLogin(t, "out@example.com", "correct horse battery")
	if w := e.do(req{method: "DELETE", path: "/v1/sessions/current", bearer: a.AccessToken}); w.Code != 204 {
		t.Fatalf("logout: %d", w.Code)
	}
	if !e.devices(t, b.AccessToken).get(b.DeviceID).HasCredential {
		t.Fatal("waiting device did not get the released slot")
	}
}

// 移除当前设备等同于登出（web 设备同样可以移除）：Cookie 清除，访问令牌立即失效。
func TestRemoveCurrentWebDevice(t *testing.T) {
	e := newEnv(t)
	e.register(t, "cur@example.com", "correct horse battery")
	w := e.login(t, "cur@example.com", "correct horse battery", webDevice)
	var s sessionBody
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	var access *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == AccessCookie {
			access = c
		}
	}
	lw := e.get("/v1/me/devices", func(r *http.Request) { r.AddCookie(access) })
	var l deviceList
	_ = json.Unmarshal(lw.Body.Bytes(), &l)
	if lw.Code != 200 || len(l.Items) != 1 || !l.Items[0].IsCurrent || l.Items[0].HasCredential || l.Items[0].Platform != "web" {
		t.Fatalf("web devices: %d %s", lw.Code, lw.Body)
	}
	rw := e.do(req{method: "DELETE", path: "/v1/me/devices/" + s.DeviceID.String(), cookies: []*http.Cookie{access}})
	if rw.Code != 204 {
		t.Fatalf("remove current: %d %s", rw.Code, rw.Body)
	}
	cleared := 0
	for _, c := range rw.Result().Cookies() {
		if c.MaxAge < 0 {
			cleared++
		}
	}
	if cleared != 2 {
		t.Fatalf("cookies not cleared: %v", rw.Result().Cookies())
	}
	if w := e.get("/v1/me", func(r *http.Request) { r.AddCookie(access) }); w.Code != 401 {
		t.Fatalf("access after removing current device: %d", w.Code)
	}
}

// 设备列表字段：平台、型号、版本、最近活跃时间（刷新时以注入的时钟更新）、最近会话的 IP 前缀（CONV-24）。
func TestListDevicesFields(t *testing.T) {
	e := newEnv(t)
	e.register(t, "list@example.com", "correct horse battery")
	a := e.appLogin(t, "list@example.com", "correct horse battery")
	l := e.devices(t, a.AccessToken)
	d := l.get(a.DeviceID)
	if l.DeviceLimit != 1 || d.Platform != "ios" || d.Model == nil || *d.Model != "iPhone17,1" || d.AppVersion == nil || *d.AppVersion != "1.4.0" ||
		d.LastSeenAt == nil || !d.LastSeenAt.Equal(t0) || d.IPPrefix == nil || *d.IPPrefix != httpx.IPPrefix(netip.MustParseAddr(e.ip)) ||
		!d.IsCurrent || d.HasCredential || d.CreatedAt.IsZero() {
		t.Fatalf("device = %+v (free account, device_limit from free_device_limit)", l)
	}
	later := e.clk.Advance(time.Hour)
	w, m := e.refresh(t, a.RefreshToken, "")
	if w.Code != 200 {
		t.Fatalf("refresh: %d %s", w.Code, w.Body)
	}
	d = e.devices(t, m["access_token"].(string)).get(a.DeviceID)
	if d.LastSeenAt == nil || !d.LastSeenAt.Equal(later) || !d.IsCurrent {
		t.Fatalf("after refresh = %+v, want last_seen_at %s", d, later)
	}
	// 没有当前权益时 device_limit 取设置项 free_device_limit。
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO settings (key, value) VALUES ('free_device_limit', '3')`); err != nil {
		t.Fatal(err)
	}
	if l := e.devices(t, m["access_token"].(string)); l.DeviceLimit != 3 {
		t.Fatalf("device_limit = %d, want 3", l.DeviceLimit)
	}
}

// M1-03 验收 3：免费套餐的 active 权益在其设备上限内下发凭据；没有权益时为 entitlement_inactive。
func TestFreePlanEntitlementIssues(t *testing.T) {
	e := newEnv(t)
	e.register(t, "free@example.com", "correct horse battery")
	none := e.appLogin(t, "free@example.com", "correct horse battery")
	if none.CredentialStatus != "entitlement_inactive" {
		t.Fatalf("no entitlement: %s", none.CredentialStatus)
	}
	if _, err := e.pool.Exec(context.Background(), `
		WITH p AS (INSERT INTO plans (name, tier, kind, status, bytes_per_cycle, device_limit) VALUES ('Free', 0, 'free', 'on_sale', 0, 2) RETURNING id)
		INSERT INTO entitlements (account_id, plan_id, status, starts_at, cycle_start, bytes_limit, device_limit, reset_policy)
		SELECT a.id, p.id, 'active', $1, $1, 0, 2, 'never' FROM accounts a, p WHERE a.email = 'free@example.com'`, t0); err != nil {
		t.Fatal(err)
	}
	if w := e.get("/v1/me", bearerAuth(none.AccessToken)); !strings.Contains(w.Body.String(), `"entitlement_status":"free"`) {
		t.Fatalf("me: %s", w.Body)
	}
	a := e.appLogin(t, "free@example.com", "correct horse battery")
	b := e.appLogin(t, "free@example.com", "correct horse battery")
	if a.CredentialStatus != "issued" || b.CredentialStatus != "device_limit_reached" {
		t.Fatalf("free plan statuses = %s, %s", a.CredentialStatus, b.CredentialStatus)
	}
	// 没有权益时登录的第一台设备在 a 登录时的分配中取得另一个名额，两个名额已满，b 等待。
	if n := e.count(t, `SELECT count(*) FROM proxy_credentials WHERE device_id IS NOT NULL AND revoked_at IS NULL`); n != 2 {
		t.Fatalf("%d device credentials, want 2 (limit of the free plan)", n)
	}
	if l := e.devices(t, a.AccessToken); l.DeviceLimit != 2 {
		t.Fatalf("device_limit = %d, want the free plan's 2", l.DeviceLimit)
	}
}

// M1-03 验收 2：并发的登录与移除不超出上限（-race 下运行）。
func TestConcurrentLoginAndRemoval(t *testing.T) {
	e := newEnv(t)
	e.register(t, "race@example.com", "correct horse battery")
	e.entitle(t, "race@example.com", 2)
	a := e.appLogin(t, "race@example.com", "correct horse battery")
	b := e.appLogin(t, "race@example.com", "correct horse battery")
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, _ := appDevice(t)
			w := e.do(req{method: "POST", path: "/v1/sessions", ip: "10.9.0." + strconv.Itoa(i+1),
				body: jsonBody(map[string]any{"email": "race@example.com", "password": "correct horse battery", "device": d})})
			if w.Code != 201 {
				t.Errorf("login: %d %s", w.Code, w.Body)
			}
		}()
	}
	for _, s := range []sessionBody{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code := e.removeDevice(s.AccessToken, s.DeviceID); code != 204 {
				t.Errorf("remove: %d", code)
			}
		}()
	}
	wg.Wait()
	if n := e.count(t, `SELECT count(*) FROM proxy_credentials WHERE device_id IS NOT NULL AND revoked_at IS NULL`); n != 2 {
		t.Fatalf("%d device credentials after concurrent logins and removals, want exactly the limit of 2", n)
	}
}
