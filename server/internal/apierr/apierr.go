// SPDX-License-Identifier: AGPL-3.0-or-later

// Package apierr 是接口错误：处理器返回 *Error，由中间件统一写成 RFC 9457 problem+json（CONV-16）。
//
// code 只能取 spec/02 2.4 错误表中的值；errors[].code 只能取第二张表中的值。
package apierr

import (
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/akari-project/panel/server/internal/httpx"
)

// FieldError 是参数错误的一项：{field, code}。
type FieldError struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

// Error 是返回给客户端的错误。Cause 只用于日志，不写入响应。
type Error struct {
	Status int
	Code   string
	Fields []FieldError
	// Ext 为附加字段，例如 mfa_required 的 challenge_id 与 methods（spec/10 AUTH-20）。
	Ext map[string]any
	// RetryAfter 大于 0 时写出 Retry-After（429、503，以及 409 conflict）。
	RetryAfter time.Duration
	Cause      error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return e.Code + ": " + e.Cause.Error()
	}
	return e.Code
}

func (e *Error) Unwrap() error { return e.Cause }

// New 返回 status 与 code 的错误。
func New(status int, code string) *Error { return &Error{Status: status, Code: code} }

// With 返回附加了扩展字段的副本。
func (e *Error) With(key string, v any) *Error {
	c := *e
	c.Ext = maps.Clone(e.Ext)
	if c.Ext == nil {
		c.Ext = map[string]any{}
	}
	c.Ext[key] = v
	return &c
}

// Invalid 返回 400 invalid_request 与逐项原因。
func Invalid(fields ...FieldError) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "invalid_request", Fields: fields}
}

// Field 是 FieldError 的简写。
func Field(field, code string) FieldError { return FieldError{Field: field, Code: code} }

// RateLimited 返回 429 rate_limited 并带 Retry-After。
func RateLimited(retry time.Duration) *Error {
	return &Error{Status: http.StatusTooManyRequests, Code: "rate_limited", RetryAfter: retry}
}

// Unavailable 返回 503 service_unavailable，用于依赖故障（spec/40 40.4）。
func Unavailable(cause error) *Error {
	return &Error{Status: http.StatusServiceUnavailable, Code: "service_unavailable", RetryAfter: 5 * time.Second, Cause: cause}
}

// 常用错误。
var (
	Unauthenticated      = New(http.StatusUnauthorized, "unauthenticated")
	Forbidden            = New(http.StatusForbidden, "forbidden")
	NotFound             = New(http.StatusNotFound, "not_found")
	Conflict             = New(http.StatusConflict, "conflict")
	InvalidState         = New(http.StatusConflict, "invalid_state")
	IdempotencyKeyReused = New(http.StatusUnprocessableEntity, "idempotency_key_reused")
	PayloadTooLarge      = New(http.StatusRequestEntityTooLarge, "payload_too_large")
)

// causeAttr 记录错误原因。PostgreSQL 错误的消息与 detail 可能含邮箱等取值，
// 只记录 SQLSTATE 与约束名（CONV-24）。
func causeAttr(err error) slog.Attr {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return slog.Group("error", slog.String("sqlstate", pg.Code), slog.String("constraint", pg.ConstraintName))
	}
	if err == nil {
		return slog.String("error", "")
	}
	return slog.String("error", err.Error())
}

// Write 把 err 写成 problem+json。非 *Error 的错误记录日志并返回 500 internal，不向客户端暴露细节。
func Write(log *slog.Logger, w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{Status: http.StatusInternalServerError, Code: "internal", Cause: err}
	}
	if e.Status >= 500 && log != nil {
		log.LogAttrs(r.Context(), slog.LevelError, "request failed",
			slog.String("request_id", httpx.RequestID(r.Context())),
			slog.String("code", e.Code),
			causeAttr(e.Cause))
	}
	body := map[string]any{}
	maps.Copy(body, e.Ext)
	body["type"] = "about:blank"
	body["title"] = http.StatusText(e.Status)
	body["status"] = e.Status
	body["code"] = e.Code
	body["request_id"] = httpx.RequestID(r.Context())
	if len(e.Fields) > 0 {
		body["errors"] = e.Fields
	}
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Set("Cache-Control", "no-store")
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int((e.RetryAfter+time.Second-1)/time.Second)))
	}
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(body)
}
