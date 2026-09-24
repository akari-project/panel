// SPDX-License-Identifier: AGPL-3.0-or-later

package httpx

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/akari-project/panel/server/internal/clock"
)

func TestAccessLogRecordsTemplateNotPath(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	mux := http.NewServeMux()
	Handle(mux, "GET /v1/configurations/{token}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	h := AccessLog(log, clock.NewFake(time.Unix(0, 0)), mux)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/configurations/SECRETTOKEN?x=1", nil))

	if strings.Contains(buf.String(), "SECRETTOKEN") {
		t.Fatalf("access log contains the actual path: %s", buf.String())
	}
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line["route"] != "GET /v1/configurations/{token}" || line["status"] != float64(http.StatusTeapot) {
		t.Errorf("log = %v", line)
	}
	id, _ := line["request_id"].(string)
	if !strings.HasPrefix(id, "req_") || rec.Header().Get("Request-Id") != id {
		t.Errorf("request id %q, header %q", id, rec.Header().Get("Request-Id"))
	}
	for _, k := range []string{"duration_ms"} {
		if _, ok := line[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
}

func TestWriteProblem(t *testing.T) {
	h := AccessLog(slog.New(slog.DiscardHandler), clock.Real{}, http.HandlerFunc(NotFound))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/nope", nil))
	if rec.Code != 404 || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("status %d, type %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Code != "not_found" || p.Status != 404 || p.Type == "" || p.Title == "" || p.RequestID == "" {
		t.Errorf("problem = %+v", p)
	}
}

func req(remote string, headers map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestProxies(t *testing.T) {
	p := Proxies{Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	for _, tc := range []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{"untrusted peer ignores header", "203.0.113.5:1234", "198.51.100.1", "203.0.113.5"},
		{"trusted peer uses header", "10.0.0.2:1234", "198.51.100.1", "198.51.100.1"},
		{"rightmost untrusted wins", "10.0.0.2:1234", "1.1.1.1, 198.51.100.1, 10.0.0.3", "198.51.100.1"},
		{"garbage falls back to last trusted", "10.0.0.2:1234", "nonsense", "10.0.0.2"},
		{"no header", "10.0.0.2:1234", "", "10.0.0.2"},
		{"mapped v4", "[::ffff:10.0.0.2]:1", "198.51.100.1", "198.51.100.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := map[string]string{}
			if tc.xff != "" {
				h["X-Forwarded-For"] = tc.xff
			}
			if got := p.ClientIP(req(tc.remote, h)).String(); got != tc.want {
				t.Errorf("ClientIP = %s, want %s", got, tc.want)
			}
		})
	}

	if (Proxies{}).IsTLS(req("10.0.0.2:1", map[string]string{"X-Forwarded-Proto": "https"})) {
		t.Error("IsTLS trusted header from untrusted peer")
	}
	if !p.IsTLS(req("10.0.0.2:1", map[string]string{"X-Forwarded-Proto": "https"})) {
		t.Error("IsTLS ignored trusted proxy header")
	}
	direct := req("203.0.113.5:1", nil)
	direct.TLS = &tls.ConnectionState{}
	if !p.IsTLS(direct) {
		t.Error("IsTLS ignored direct TLS")
	}
}

func TestIPPrefix(t *testing.T) {
	if got := IPPrefix(netip.MustParseAddr("198.51.100.77")); got != "198.51.100.0/24" {
		t.Errorf("v4 = %s", got)
	}
	if got := IPPrefix(netip.MustParseAddr("2001:db8:1234:5678::1")); got != "2001:db8:1234::/48" {
		t.Errorf("v6 = %s", got)
	}
}

func TestHealthHandler(t *testing.T) {
	for _, tc := range []struct {
		ping func(context.Context) error
		want string
	}{
		{func(context.Context) error { return nil }, "ok"},
		{func(context.Context) error { return errors.New("down") }, "unavailable"},
	} {
		rec := httptest.NewRecorder()
		HealthHandler([]string{"api"}, "dev", tc.ping).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		var h Health
		if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || h.Status != "ok" || h.Database != tc.want || h.Roles[0] != "api" {
			t.Errorf("health = %d %+v", rec.Code, h)
		}
	}
}
