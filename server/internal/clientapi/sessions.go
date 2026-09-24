// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/clientapi/gen"
	"github.com/akari-project/panel/server/internal/httpx"
	"github.com/akari-project/panel/server/internal/session"
)

// RefreshCookie 是用户中心的刷新令牌 Cookie（AUTH-08）。
const RefreshCookie = "__Host-refresh_token"

var noStore = "no-store"

// setTokenCookies 以 HttpOnly、Secure、SameSite=Strict、Path=/ 的 __Host- Cookie 下发令牌（AUTH-08）。
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

func deviceFromAPI(d gen.DeviceInfo) session.Device {
	out := session.Device{Platform: string(d.Platform), Model: d.Model, AppVersion: d.AppVersion, PublicKey: d.PublicKey}
	if d.DeviceId != nil && d.DeviceProof != nil {
		id := uuid.UUID(*d.DeviceId)
		out.ReuseID = &id
		out.Proof = &session.Proof{Nonce: d.DeviceProof.Nonce, Signature: d.DeviceProof.Signature}
	}
	return out
}

// CreateSession 登录并注册设备（AUTH-09、AUTH-10、AUTH-20）。web 设备的令牌以 Cookie 下发，响应体不含令牌。
func (s *Server) CreateSession(ctx context.Context, req gen.CreateSessionRequestObject) (gen.CreateSessionResponseObject, error) {
	raw := info(ctx).Body
	var union gen.CreateSessionJSONBody
	if err := union.UnmarshalJSON(raw); err != nil {
		return nil, apierr.Invalid()
	}
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
	body, err := union.AsPasswordLogin()
	if err != nil {
		return nil, apierr.Invalid()
	}
	res, err := s.d.Sessions.PasswordLogin(ctx, session.Login{
		Email: string(body.Email), Password: body.Password, Device: deviceFromAPI(body.Device),
		IP: ipSubject(ri.IP), IPPrefix: httpx.IPPrefix(ri.IP), UserAgent: ri.UserAgent,
	})
	if err != nil {
		return nil, err
	}
	return s.sessionResponse(ctx, res), nil
}

// secondStep 是登录第二步（AUTH-20）：challenge_id、设备，以及 totp_code 或 recovery_code。
// Passkey 在 M4 实现。
func (s *Server) secondStep(ctx context.Context, raw []byte) (gen.CreateSessionResponseObject, error) {
	var body struct {
		ChallengeID       uuid.UUID       `json:"challenge_id"`
		Device            gen.DeviceInfo  `json:"device"`
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
		return nil, apierr.Invalid(apierr.Field("webauthn_assertion", "not_allowed"))
	}
	ri := info(ctx)
	res, err := s.d.Sessions.MFALogin(ctx, session.MFALogin{
		ChallengeID: body.ChallengeID, Device: deviceFromAPI(body.Device), TOTPCode: body.TotpCode, RecoveryCode: body.RecoveryCode,
		IP: ipSubject(ri.IP), IPPrefix: httpx.IPPrefix(ri.IP), UserAgent: ri.UserAgent,
	})
	if err != nil {
		return nil, err
	}
	return s.sessionResponse(ctx, res), nil
}

func (s *Server) sessionResponse(ctx context.Context, res session.Result) gen.CreateSession201JSONResponse {
	out := gen.Session{
		DeviceId:         res.DeviceID,
		TokenType:        "Bearer",
		ExpiresIn:        int(token.TTL.Seconds()),
		CredentialStatus: gen.CredentialStatus(res.CredentialStatus),
	}
	if res.IsWeb {
		setTokenCookies(ctx, res.Tokens, s.d.Clock.Now())
	} else {
		out.AccessToken, out.RefreshToken = &res.Access, &res.Refresh
	}
	return gen.CreateSession201JSONResponse{Body: out}
}

// CreateSessionNonce 取得一次性 nonce，供设备复用时签名（AUTH-10）。响应含秘密值，不接受 Idempotency-Key（CONV-12）。
func (s *Server) CreateSessionNonce(ctx context.Context, _ gen.CreateSessionNonceRequestObject) (gen.CreateSessionNonceResponseObject, error) {
	n, err := s.d.Sessions.NewNonce(ctx)
	if err != nil {
		return nil, err
	}
	resp := gen.CreateSessionNonce201JSONResponse{Headers: gen.CreateSessionNonce201ResponseHeaders{CacheControl: &noStore}}
	resp.Body.Nonce = n
	resp.Body.ExpiresAt = s.d.Clock.Now().Add(session.NonceTTL)
	return resp, nil
}

// DeleteCurrentSession 登出：吊销本会话、本设备及其代理凭据（AUTH-10）。
func (s *Server) DeleteCurrentSession(ctx context.Context, _ gen.DeleteCurrentSessionRequestObject) (gen.DeleteCurrentSessionResponseObject, error) {
	p, _ := auth.FromContext(ctx)
	if err := s.d.Sessions.Logout(ctx, p); err != nil {
		return nil, err
	}
	clearTokenCookies(ctx)
	return gen.DeleteCurrentSession204Response{}, nil
}

func oauthError(code string) gen.IssueToken400JSONResponse {
	return gen.IssueToken400JSONResponse{OAuthErrorJSONResponse: gen.OAuthErrorJSONResponse{Error: gen.OAuthErrorBodyError(code)}}
}

// IssueToken 是 OAuth 令牌端点（AUTH-07、AUTH-24）。错误按 RFC 6749 §5.2 返回（CONV-16 的例外）；
// 限流与依赖故障仍为 problem+json。浏览器省略 refresh_token，由 Cookie 携带，新令牌同样以 Cookie 下发。
func (s *Server) IssueToken(ctx context.Context, req gen.IssueTokenRequestObject) (gen.IssueTokenResponseObject, error) {
	if req.Body == nil {
		return oauthError("invalid_request"), nil
	}
	switch req.Body.GrantType {
	case gen.IssueTokenFormdataBodyGrantTypeRefreshToken:
	case gen.IssueTokenFormdataBodyGrantTypeUrnIetfParamsOauthGrantTypeDeviceCode:
		// 设备授权与扫码登录（AUTH-24）不在 M1-01 范围内。
		return oauthError("unsupported_grant_type"), nil
	default:
		return oauthError("unsupported_grant_type"), nil
	}
	refresh, web := "", false
	if req.Body.RefreshToken != nil {
		refresh = *req.Body.RefreshToken
	} else if c := cookie(ctx, RefreshCookie); c != "" {
		refresh, web = c, true
	}
	ri := info(ctx)
	t, err := s.d.Sessions.Refresh(ctx, refresh, httpx.IPPrefix(ri.IP), ri.UserAgent)
	var oe *session.OAuthError
	if errors.As(err, &oe) {
		// 并发刷新未取得缓存时，Cookie 中可能已是另一请求刚写入的新令牌，不能清除。
		if web && !oe.Retry {
			clearTokenCookies(ctx)
		}
		return oauthError(oe.Code), nil
	}
	if err != nil {
		return nil, err
	}
	pair := gen.TokenPair{TokenType: "Bearer", ExpiresIn: int(token.TTL.Seconds()), DeviceId: &t.DeviceID}
	if web {
		// 浏览器的令牌只在 Cookie 中（AUTH-08）；契约中这两个字段为必填，以空串占位。
		setTokenCookies(ctx, t, s.d.Clock.Now())
	} else {
		pair.AccessToken, pair.RefreshToken = t.Access, t.Refresh
	}
	return gen.IssueToken200JSONResponse{Body: pair, Headers: gen.IssueToken200ResponseHeaders{CacheControl: &noStore}}, nil
}
