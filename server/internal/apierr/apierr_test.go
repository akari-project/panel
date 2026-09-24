// SPDX-License-Identifier: AGPL-3.0-or-later

package apierr

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, err error) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	Write(slog.New(slog.DiscardHandler), w, httptest.NewRequest("GET", "/v1/x", nil), err)
	var body map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &body); e != nil {
		t.Fatal(e)
	}
	return w, body
}

// CONV-16：problem+json 必含 type、title、status、code、request_id；参数错误附 errors[]。
func TestWriteProblem(t *testing.T) {
	w, body := write(t, Invalid(Field("email", "invalid_format")))
	if w.Code != 400 || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("status %d, content-type %q", w.Code, w.Header().Get("Content-Type"))
	}
	for _, k := range []string{"type", "title", "status", "code", "request_id"} {
		if _, ok := body[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	errs, _ := body["errors"].([]any)
	if body["code"] != "invalid_request" || len(errs) != 1 || errs[0].(map[string]any)["field"] != "email" {
		t.Fatalf("body = %v", body)
	}
}

func TestExtensionsAndRetryAfter(t *testing.T) {
	w, body := write(t, New(401, "mfa_required").With("challenge_id", "c1").With("methods", []string{"totp"}))
	if w.Code != 401 || body["challenge_id"] != "c1" || body["code"] != "mfa_required" {
		t.Fatalf("body = %v", body)
	}
	w, _ = write(t, RateLimited(1500*time.Millisecond))
	if w.Code != 429 || w.Header().Get("Retry-After") != "2" {
		t.Fatalf("status %d retry-after %q", w.Code, w.Header().Get("Retry-After"))
	}
	// 扩展字段不能覆盖必含字段。
	_, body = write(t, New(409, "conflict").With("code", "x").With("status", 1))
	if body["code"] != "conflict" || body["status"] != float64(409) {
		t.Fatalf("body = %v", body)
	}
}

// 非 *Error 的错误返回 500 internal，不暴露细节。
func TestInternal(t *testing.T) {
	w, body := write(t, errors.New("pq: secret detail"))
	if w.Code != http.StatusInternalServerError || body["code"] != "internal" {
		t.Fatalf("status %d body %v", w.Code, body)
	}
	if s := w.Body.String(); len(s) == 0 || strings.Contains(s, "secret detail") {
		t.Fatalf("body leaks cause: %s", s)
	}
}
