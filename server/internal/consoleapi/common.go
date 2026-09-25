// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/auth"
)

// MaxReason 是操作原因的上限（Unicode 码点，spec/31 CON-03）。
const MaxReason = 500

// bodyReason 校验请求体中的 reason：1 到 500 个码点（AUTH-19、CON-03）。
func bodyReason(reason string) (string, error) {
	switch n := utf8.RuneCountInString(reason); {
	case strings.TrimSpace(reason) == "":
		return "", apierr.Invalid(apierr.Field("reason", "required"))
	case n > MaxReason:
		return "", apierr.Invalid(apierr.Field("reason", "too_long"))
	}
	return reason, nil
}

// headerReason 校验 DELETE 请求的 Audit-Reason：UTF-8 百分号编码，解码后 1 到 500 个码点（CON-03）。
// 缺少、无法解码或超长返回 400，errors[].field 为 Audit-Reason。
func headerReason(raw *string) (string, error) {
	const field = "Audit-Reason"
	if raw == nil || *raw == "" {
		return "", apierr.Invalid(apierr.Field(field, "required"))
	}
	s, err := url.PathUnescape(*raw)
	if err != nil || !utf8.ValidString(s) {
		return "", apierr.Invalid(apierr.Field(field, "invalid_format"))
	}
	switch n := utf8.RuneCountInString(s); {
	case strings.TrimSpace(s) == "":
		return "", apierr.Invalid(apierr.Field(field, "required"))
	case n > MaxReason:
		return "", apierr.Invalid(apierr.Field(field, "too_long"))
	}
	return s, nil
}

// requireStepUp 校验敏感操作的 Mfa-Assertion（AUTH-19）：缺少、过期或不属于当前会话链返回 401 mfa_required。
// 敏感操作的处理器在校验参数与原因（400）之后、产生任何副作用之前调用（校验顺序见 AUTH-19，CONV-12）。
func (s *Server) requireStepUp(ctx context.Context) error {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return apierr.Unauthenticated
	}
	return s.d.Sessions.CheckStepUp(ctx, p, header(ctx, "Mfa-Assertion"))
}

// 分页（CONV-11）：limit 默认 50，最大 200。
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

func pageLimit(limit *int) (int32, error) {
	if limit == nil {
		return DefaultLimit, nil
	}
	if *limit < 1 || *limit > MaxLimit {
		return 0, apierr.Invalid(apierr.Field("limit", "out_of_range"))
	}
	return int32(*limit), nil
}

// encodeCursor 把排序键编码为不透明的 base64url 游标（CONV-11）。
func encodeCursor(v any) *string {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	s := base64.RawURLEncoding.EncodeToString(b)
	return &s
}

// decodeCursor 解码游标；非法游标返回 400 invalid_request（CONV-11）。
func decodeCursor(c *string, v any) (bool, error) {
	if c == nil || *c == "" {
		return false, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(*c)
	if err != nil {
		return false, apierr.Invalid(apierr.Field("cursor", "invalid_format"))
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return false, apierr.Invalid(apierr.Field("cursor", "invalid_format"))
	}
	return true, nil
}
