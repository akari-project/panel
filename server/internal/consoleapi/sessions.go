// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/consoleapi/gen"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/httpx"
	"github.com/akari-project/panel/server/internal/rbac"
	"github.com/akari-project/panel/server/internal/session"
)

var noStore = "no-store"

// setTokenCookies 以 HttpOnly、Secure、SameSite=Strict、Path=/ 的 __Host- Cookie 下发管理令牌（AUTH-08、AUTH-21）。
func setTokenCookies(ctx context.Context, t session.Tokens, now time.Time) {
	setCookie(ctx, &http.Cookie{Name: AccessCookie, Value: t.Access, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(t.AccessExpires.Sub(now).Seconds())})
	setCookie(ctx, &http.Cookie{Name: RefreshCookie, Value: t.Refresh, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(t.RefreshExpires.Sub(now).Seconds())})
}

func clearTokenCookies(ctx context.Context) {
	for _, name := range []string{AccessCookie, RefreshCookie} {
		setCookie(ctx, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: true,
			SameSite: http.SameSiteStrictMode, MaxAge: -1})
	}
}

// staffMe 返回当前管理员，含站点结算货币（尚未初始化时为 null，CONV-08）。
func (s *Server) staffMe(ctx context.Context, st rbac.Staff) (gen.StaffMe, error) {
	perms := make([]gen.Permission, len(st.Permissions))
	for i, p := range st.Permissions {
		perms[i] = gen.Permission(p)
	}
	currency, err := s.catalog.SiteCurrency(ctx)
	if err != nil {
		return gen.StaffMe{}, err
	}
	return gen.StaffMe{
		AccountId: st.AccountID, Email: openapi_types.Email(st.Email), Roles: st.Roles, Permissions: perms,
		IsSuperadmin: st.IsSuperadmin(), HasTotp: st.HasTOTP, HasPasskey: st.HasPasskey,
		SiteCurrency: nullableString(currency),
	}, nil
}

// CreateSession 是管理员两步登录（AUTH-20、AUTH-21）。第一步总是以错误结束（成功时为 401 mfa_required）；
// 第二步成功后以 Cookie 下发受众为 console 的令牌，响应体不含令牌（AUTH-08）。
func (s *Server) CreateSession(ctx context.Context, _ gen.CreateSessionRequestObject) (gen.CreateSessionResponseObject, error) {
	raw := info(ctx).Body
	var probe struct {
		ChallengeID *string `json:"challenge_id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, apierr.Invalid()
	}
	ri := info(ctx)
	if probe.ChallengeID != nil {
		return s.secondStep(ctx, raw)
	}
	var body gen.PasswordLogin
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || body.Email == "" {
		return nil, apierr.Invalid()
	}
	return nil, s.d.Sessions.ConsolePasswordLogin(ctx, session.ConsoleLogin{
		Email: string(body.Email), Password: body.Password,
		IP: ipSubject(ri.IP), IPPrefix: httpx.IPPrefix(ri.IP), UserAgent: ri.UserAgent,
	})
}

func (s *Server) secondStep(ctx context.Context, raw []byte) (gen.CreateSessionResponseObject, error) {
	var body struct {
		ChallengeID       uuid.UUID       `json:"challenge_id"`
		TotpCode          string          `json:"totp_code"`
		RecoveryCode      string          `json:"recovery_code"`
		WebauthnAssertion json.RawMessage `json:"webauthn_assertion"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return nil, apierr.Invalid()
	}
	if len(body.WebauthnAssertion) > 0 {
		return nil, apierr.Invalid(apierr.Field("webauthn_assertion", "not_allowed")) // Passkey 在 M4
	}
	ri := info(ctx)
	res, err := s.d.Sessions.ConsoleCompleteLogin(ctx, session.ConsoleMFALogin{
		ChallengeID: body.ChallengeID, TOTPCode: body.TotpCode, RecoveryCode: body.RecoveryCode,
		IP: ipSubject(ri.IP), IPPrefix: httpx.IPPrefix(ri.IP), UserAgent: ri.UserAgent,
	})
	if err != nil {
		return nil, err
	}
	st, err := rbac.Load(ctx, sqlc.New(s.d.Pool), res.AccountID)
	if err != nil {
		return nil, err
	}
	me, err := s.staffMe(ctx, st)
	if err != nil {
		return nil, err
	}
	setTokenCookies(ctx, res.Tokens, s.d.Clock.Now())
	out := gen.ConsoleSession{
		SessionId: res.SessionID, AccessExpiresAt: res.AccessExpires, RefreshExpiresAt: res.AbsoluteExpires, Staff: me,
	}
	if res.RecoveryCodes != nil {
		out.RecoveryCodes = &res.RecoveryCodes
	}
	return gen.CreateSession201JSONResponse{Body: out}, nil
}

// DeleteCurrentSession 登出：吊销当前管理会话链，写审计 session.delete，并清除 Cookie。
func (s *Server) DeleteCurrentSession(ctx context.Context, _ gen.DeleteCurrentSessionRequestObject) (gen.DeleteCurrentSessionResponseObject, error) {
	p, _ := auth.FromContext(ctx)
	if err := s.d.Sessions.ConsoleLogout(ctx, p); err != nil {
		return nil, err
	}
	clearTokenCookies(ctx)
	return gen.DeleteCurrentSession204Response{}, nil
}

func oauthError(code string) gen.RefreshToken400JSONResponse {
	return gen.RefreshToken400JSONResponse{Error: gen.OAuthErrorError(code)}
}

// RefreshToken 轮换管理会话的刷新令牌（AUTH-07、AUTH-21）。刷新令牌只由 Cookie 携带，新令牌同样以 Cookie 下发。
// 错误按 RFC 6749 §5.2 返回（CONV-16 的例外）。
func (s *Server) RefreshToken(ctx context.Context, req gen.RefreshTokenRequestObject) (gen.RefreshTokenResponseObject, error) {
	if req.Body == nil || req.Body.GrantType != gen.RefreshTokenFormdataBodyGrantTypeRefreshToken {
		return oauthError("unsupported_grant_type"), nil
	}
	ri := info(ctx)
	t, err := s.d.Sessions.ConsoleRefresh(ctx, cookie(ctx, RefreshCookie), httpx.IPPrefix(ri.IP), ri.UserAgent)
	var oe *session.OAuthError
	if errors.As(err, &oe) {
		// 并发刷新未取得缓存时，Cookie 中可能已是另一请求刚写入的新令牌，不能清除。
		if !oe.Retry {
			clearTokenCookies(ctx)
		}
		return oauthError(oe.Code), nil
	}
	if err != nil {
		return nil, err
	}
	setTokenCookies(ctx, t, s.d.Clock.Now())
	return gen.RefreshToken200JSONResponse{
		Body:    gen.SessionRefresh{SessionId: t.SessionID, AccessExpiresAt: t.AccessExpires, RefreshExpiresAt: t.AbsoluteExpires},
		Headers: gen.RefreshToken200ResponseHeaders{CacheControl: &noStore},
	}, nil
}

// GetCurrentStaff 返回当前管理员的角色与展开后的权限（AUTH-17）。
func (s *Server) GetCurrentStaff(ctx context.Context, _ gen.GetCurrentStaffRequestObject) (gen.GetCurrentStaffResponseObject, error) {
	me, err := s.staffMe(ctx, staff(ctx))
	if err != nil {
		return nil, err
	}
	return gen.GetCurrentStaff200JSONResponse(me), nil
}

// CreateStepUp 以 TOTP 完成重新验证，返回 5 分钟有效的 Mfa-Assertion（AUTH-19）。恢复码与 Passkey（M4 前）不可用。
func (s *Server) CreateStepUp(ctx context.Context, req gen.CreateStepUpRequestObject) (gen.CreateStepUpResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	if req.Body.WebauthnAssertion != nil {
		return nil, apierr.Invalid(apierr.Field("webauthn_assertion", "not_allowed"))
	}
	code := ""
	if req.Body.TotpCode != nil {
		code = *req.Body.TotpCode
	}
	p, _ := auth.FromContext(ctx)
	assertion, expires, err := s.d.Sessions.StepUp(ctx, p, code)
	if err != nil {
		return nil, err
	}
	return gen.CreateStepUp201JSONResponse{MfaAssertion: assertion, ExpiresAt: expires}, nil
}
