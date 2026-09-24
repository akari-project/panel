// SPDX-License-Identifier: AGPL-3.0-or-later

package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/testdb"
)

var t0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

type fixture struct {
	store Store
	clk   *clock.Fake
	calls atomic.Int32
	// status 是处理器返回的状态码。
	status atomic.Int32
	// hold 非空时处理器阻塞到其关闭，用于模拟首个请求仍在处理。
	hold atomic.Pointer[chan struct{}]
}

func newFixture(t *testing.T) *fixture {
	clk := clock.NewFake(t0)
	f := &fixture{store: Store{Pool: testdb.New(t), Clock: clk, HashKey: []byte("test-hash-key")}, clk: clk}
	f.status.Store(201)
	return f
}

func (f *fixture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.calls.Add(1)
		if h := f.hold.Load(); h != nil {
			<-*h
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/v1/orders/1")
		w.Header().Set("Set-Cookie", "x=1")
		w.WriteHeader(int(f.status.Load()))
		_ = json.NewEncoder(w).Encode(map[string]int32{"call": n})
	})
}

func (f *fixture) do(key, body string, account *uuid.UUID) *httptest.ResponseRecorder {
	return f.doRoute("POST /v1/orders", key, body, account)
}

func (f *fixture) doRoute(route, key, body string, account *uuid.UUID) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/orders", strings.NewReader(body))
	if key != "" {
		r.Header.Set(Header, key)
	}
	w := httptest.NewRecorder()
	f.store.Serve(w, r, route, account, f.handler(), func(w http.ResponseWriter, r *http.Request, err error) {
		apierr.Write(nil, w, r, err)
	})
	return w
}

func code(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct{ Code string }
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	return p.Code
}

// 相同的键与请求体返回相同结果，只执行一次；重放不带每次请求各自的响应头。
func TestReplay(t *testing.T) {
	f := newFixture(t)
	acct := uuid.New()
	key := uuid.NewString()
	first := f.do(key, `{"a":1}`, &acct)
	second := f.do(key, `{"a":1}`, &acct)
	if f.calls.Load() != 1 {
		t.Fatalf("handler ran %d times", f.calls.Load())
	}
	if second.Code != 201 || second.Body.String() != first.Body.String() || second.Header().Get("Location") != "/v1/orders/1" {
		t.Fatalf("replay = %d %q %v", second.Code, second.Body.String(), second.Header())
	}
	if second.Header().Get("Set-Cookie") != "" {
		t.Fatal("Set-Cookie replayed")
	}
	// 没有请求头时每次都执行。
	f.do("", `{"a":1}`, &acct)
	f.do("", `{"a":1}`, &acct)
	if f.calls.Load() != 3 {
		t.Fatalf("handler ran %d times without key", f.calls.Load())
	}
}

// 相同的键、不同的请求体或路由：422 idempotency_key_reused。
func TestReused(t *testing.T) {
	f := newFixture(t)
	acct := uuid.New()
	key := uuid.NewString()
	f.do(key, `{"a":1}`, &acct)
	if w := f.do(key, `{"a":2}`, &acct); w.Code != 422 || code(t, w) != "idempotency_key_reused" {
		t.Fatalf("different body: %d %s", w.Code, w.Body)
	}
	if w := f.doRoute("POST /v1/me/redemptions", key, `{"a":1}`, &acct); w.Code != 422 {
		t.Fatalf("different route: %d %s", w.Code, w.Body)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("handler ran %d times", f.calls.Load())
	}
}

// 作用域：已认证为“账号 + 键”，未认证为“路由 + 键”。
func TestScope(t *testing.T) {
	f := newFixture(t)
	a, b := uuid.New(), uuid.New()
	key := uuid.NewString()
	f.do(key, `{}`, &a)
	f.do(key, `{}`, &b)
	f.do(key, `{}`, nil)
	f.doRoute("POST /v1/accounts", key, `{}`, nil)
	if f.calls.Load() != 4 {
		t.Fatalf("handler ran %d times, want 4 independent scopes", f.calls.Load())
	}
	f.do(key, `{}`, nil)
	if f.calls.Load() != 4 {
		t.Fatal("unauthenticated replay executed again")
	}
}

// 首个请求仍在处理时：409 conflict 并带 Retry-After。
func TestInProgress(t *testing.T) {
	f := newFixture(t)
	hold := make(chan struct{})
	f.hold.Store(&hold)
	acct := uuid.New()
	key := uuid.NewString()
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- f.do(key, `{}`, &acct) }()
	for f.calls.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	w := f.do(key, `{}`, &acct)
	if w.Code != 409 || code(t, w) != "conflict" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("concurrent: %d %s %v", w.Code, w.Body, w.Header())
	}
	close(hold)
	if first := <-done; first.Code != 201 {
		t.Fatalf("first: %d", first.Code)
	}
}

// 5xx 不缓存，允许用同一键重试；4xx 缓存。
func TestServerErrorsNotCached(t *testing.T) {
	f := newFixture(t)
	acct := uuid.New()
	key := uuid.NewString()
	f.status.Store(503)
	f.do(key, `{}`, &acct)
	f.status.Store(201)
	if w := f.do(key, `{}`, &acct); w.Code != 201 || f.calls.Load() != 2 {
		t.Fatalf("retry after 5xx: %d, calls %d", w.Code, f.calls.Load())
	}

	key = uuid.NewString()
	f.status.Store(409)
	f.do(key, `{}`, &acct)
	f.status.Store(201)
	if w := f.do(key, `{}`, &acct); w.Code != 409 || f.calls.Load() != 3 {
		t.Fatalf("4xx not replayed: %d, calls %d", w.Code, f.calls.Load())
	}
}

// 处理器 panic 时删除记录，允许重试。
func TestPanicReleasesKey(t *testing.T) {
	f := newFixture(t)
	acct := uuid.New()
	key := uuid.NewString()
	r := httptest.NewRequest("POST", "/v1/orders", strings.NewReader(`{}`))
	r.Header.Set(Header, key)
	func() {
		defer func() { _ = recover() }()
		f.store.Serve(httptest.NewRecorder(), r, "POST /v1/orders", &acct,
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }), nil)
	}()
	if w := f.do(key, `{}`, &acct); w.Code != 201 || f.calls.Load() != 1 {
		t.Fatalf("after panic: %d, calls %d", w.Code, f.calls.Load())
	}
}

// 24 小时后过期，同一键重新执行；过期记录由 Sweep 删除。
func TestExpiryAndSweep(t *testing.T) {
	f := newFixture(t)
	acct := uuid.New()
	key := uuid.NewString()
	f.do(key, `{}`, &acct)
	f.clk.Advance(TTL)
	if w := f.do(key, `{"changed":true}`, &acct); w.Code != 201 || f.calls.Load() != 2 {
		t.Fatalf("after expiry: %d, calls %d", w.Code, f.calls.Load())
	}
	f.do(uuid.NewString(), `{}`, &acct)
	f.clk.Advance(TTL)
	n, err := f.store.Sweep(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
}

// 首个请求所在进程退出、记录一直处于处理中：超过 Abandoned 后允许重新执行。
func TestAbandoned(t *testing.T) {
	f := newFixture(t)
	hold := make(chan struct{})
	defer close(hold)
	f.hold.Store(&hold)
	acct := uuid.New()
	key := uuid.NewString()
	go f.do(key, `{}`, &acct)
	for f.calls.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	f.clk.Advance(Abandoned - time.Second)
	if w := f.do(key, `{}`, &acct); w.Code != 409 {
		t.Fatalf("before abandoned: %d", w.Code)
	}
	f.clk.Advance(time.Second)
	f.hold.Store(nil)
	if w := f.do(key, `{}`, &acct); w.Code != 201 {
		t.Fatalf("after abandoned: %d %s", w.Code, w.Body)
	}
}

// 请求体可能含密码：只保存带密钥的 HMAC，不保存可离线穷举的 SHA-256。
func TestRequestHashIsKeyed(t *testing.T) {
	f := newFixture(t)
	body := `{"password":"hunter2"}`
	f.do(uuid.NewString(), body, nil)
	var stored []byte
	if err := f.store.Pool.QueryRow(context.Background(), `SELECT request_hash FROM idempotency_keys`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	plain := sha256.Sum256([]byte(body))
	if len(stored) != sha256.Size || bytes.Equal(stored, plain[:]) {
		t.Fatalf("request_hash is unkeyed SHA-256")
	}
}

func TestInvalidKey(t *testing.T) {
	f := newFixture(t)
	w := f.do("not-a-uuid", `{}`, nil)
	if w.Code != 400 || code(t, w) != "invalid_request" || !strings.Contains(w.Body.String(), Header) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if f.calls.Load() != 0 {
		t.Fatal("handler ran")
	}
}
