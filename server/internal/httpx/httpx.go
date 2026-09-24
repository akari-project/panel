// SPDX-License-Identifier: AGPL-3.0-or-later

// Package httpx 是各角色共用的 HTTP 基础设施：请求 ID、访问日志（CONV-23）、
// problem+json 错误（CONV-16）、可信代理下的客户端地址与 TLS 判定（spec/40 DEP-13）。
package httpx

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/logging"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyRoute
)

// RequestID 返回本请求的 ID。
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(keyRequestID).(string)
	return s
}

// SetRoute 记录本请求的路由模板，访问日志中的 route 字段取此值（CONV-23 只记录模板，不记录实际路径）。
func SetRoute(r *http.Request, route string) {
	if p, ok := r.Context().Value(keyRoute).(*string); ok {
		*p = route
	}
}

// Handle 在 mux 上注册 pattern，并把 pattern 记为路由模板。
func Handle(mux *http.ServeMux, pattern string, h http.Handler) {
	mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetRoute(r, pattern)
		h.ServeHTTP(w, r)
	}))
}

// newRequestID 返回 req_ 加 UUIDv7 的十六进制，与 OpenAPI 示例的形式一致。
func newRequestID() string {
	id, err := uuid.NewV7()
	if err != nil {
		id = uuid.New()
	}
	return "req_" + hex.EncodeToString(id[:])
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// AccessLog 分配请求 ID 并记录访问日志。日志不含实际路径、查询串、客户端 IP（CONV-24）。
func AccessLog(log *slog.Logger, clk clock.Clock, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := clk.Now()
		id := newRequestID()
		route := ""
		ctx := context.WithValue(r.Context(), keyRequestID, id)
		ctx = context.WithValue(ctx, keyRoute, &route)
		sw := &statusWriter{ResponseWriter: w}
		sw.Header().Set("Request-Id", id)
		next.ServeHTTP(sw, r.WithContext(ctx))
		if route == "" {
			route = r.Method + " (unmatched)"
		}
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		log.LogAttrs(ctx, slog.LevelInfo, "request",
			slog.String(logging.KeyRequestID, id),
			slog.String(logging.KeyRoute, route),
			slog.Int(logging.KeyStatus, sw.status),
			slog.Int64(logging.KeyDurationMS, clk.Now().Sub(start).Milliseconds()),
		)
	})
}

// Problem 是 RFC 9457 错误体（CONV-16）。
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Code      string `json:"code"`
	RequestID string `json:"request_id"`
}

// WriteProblem 写出 problem+json 错误。
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{
		Type:      "about:blank",
		Title:     http.StatusText(status),
		Status:    status,
		Code:      code,
		RequestID: RequestID(r.Context()),
	})
}

// NotFound 返回 404 not_found（CONV-15）。
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, http.StatusNotFound, "not_found")
}

// Proxies 按可信代理列表解析客户端地址与协议（DEP-13）。
type Proxies struct {
	Trusted []netip.Prefix
}

func (p Proxies) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, pf := range p.Trusted {
		if pf.Contains(a) {
			return true
		}
	}
	return false
}

func peerAddr(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		a, _ := netip.ParseAddr(r.RemoteAddr)
		return a.Unmap()
	}
	return ap.Addr().Unmap()
}

// ClientIP 返回客户端地址：对端是可信代理时，取 X-Forwarded-For 中从右往左第一个不可信的地址；
// 否则一律使用 TCP 对端地址。
func (p Proxies) ClientIP(r *http.Request) netip.Addr {
	peer := peerAddr(r)
	if !p.trusted(peer) {
		return peer
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return peer
		}
		a = a.Unmap()
		if !p.trusted(a) {
			return a
		}
		peer = a
	}
	return peer
}

// IsTLS 报告请求是否经 TLS 到达：直接 TLS，或来自可信代理且 X-Forwarded-Proto 为 https。
func (p Proxies) IsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return p.trusted(peerAddr(r)) && strings.EqualFold(strings.TrimSpace(firstValue(r.Header.Get("X-Forwarded-Proto"))), "https")
}

func firstValue(v string) string {
	s, _, _ := strings.Cut(v, ",")
	return s
}

// IPPrefix 返回用于记录的地址前缀：IPv4 为 /24，IPv6 为 /48（CONV-24）。
func IPPrefix(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	bits := 48
	if a.Is4() {
		bits = 24
	}
	pf, _ := a.Prefix(bits)
	return pf.String()
}

// Health 描述 /healthz 的响应。
type Health struct {
	Status   string   `json:"status"`
	Roles    []string `json:"roles"`
	Version  string   `json:"version"`
	Database string   `json:"database"`
}

// HealthHandler 返回 /healthz 处理器。这是存活探针：数据库不可用时仍返回 200，
// 在 database 字段中报告 unavailable，避免依赖故障导致实例被反复重启（spec/40 40.4）。
func HealthHandler(roles []string, version string, ping func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := Health{Status: "ok", Roles: roles, Version: version, Database: "ok"}
		if ping != nil {
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			defer cancel()
			if err := ping(ctx); err != nil {
				h.Database = "unavailable"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(h)
	})
}
