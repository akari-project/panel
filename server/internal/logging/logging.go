// SPDX-License-Identifier: AGPL-3.0-or-later

// Package logging 建立 slog 结构化日志（CONV-23），并对疑似敏感的字段做兜底脱敏（CONV-24）。
//
// 兜底脱敏不能代替调用方的责任：不要把密码、令牌、导出链接、代理凭据、密钥、完整 IP 写进日志。
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// 标准字段名（CONV-23）。
const (
	KeyRequestID  = "request_id"
	KeyAccountID  = "account_id"
	KeyNodeID     = "node_id"
	KeyRoute      = "route"
	KeyStatus     = "status"
	KeyDurationMS = "duration_ms"
	KeyRole       = "role"
)

// Redacted 是被脱敏字段的替换值。
const Redacted = "[REDACTED]"

// sensitiveKeys 中的片段出现在字段名中时，该字段的值被替换（大小写不敏感）。
var sensitiveKeys = []string{"password", "passwd", "token", "secret", "credential", "private_key", "psk", "api_key", "authorization", "cookie"}

// New 按级别与格式创建 logger。
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("logging: level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lv, ReplaceAttr: redact}
	var h slog.Handler
	switch format {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("logging: unknown format %q", format)
	}
	return slog.New(h), nil
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if IsSensitiveKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	return a
}

// IsSensitiveKey 报告字段名是否被视为敏感。
func IsSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}
