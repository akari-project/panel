// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"
	"strings"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/akari-project/panel/server/internal/account"
	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/clientapi/gen"
)

var accepted = gen.Accepted{Status: "accepted"}

// requestLocale 取请求体中的语言，其次取 Accept-Language 的第一个标签。
func requestLocale(ctx context.Context, body *string) string {
	if body != nil && account.ValidLocale(*body) {
		return *body
	}
	first, _, _ := strings.Cut(info(ctx).AcceptLanguage, ",")
	first, _, _ = strings.Cut(first, ";")
	if first = strings.TrimSpace(first); account.ValidLocale(first) {
		return first
	}
	return ""
}

func principalID(ctx context.Context) *uuid.UUID {
	if p, ok := auth.FromContext(ctx); ok {
		return &p.AccountID
	}
	return nil
}

func emailPtr(e *openapi_types.Email) *string {
	if e == nil {
		return nil
	}
	s := string(*e)
	return &s
}

// CreateAccount 注册（spec/10 AUTH-01、AUTH-02）。邮箱已注册时同样返回 202。
func (s *Server) CreateAccount(ctx context.Context, req gen.CreateAccountRequestObject) (gen.CreateAccountResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	b := req.Body
	err := s.d.Accounts.Register(ctx, account.Registration{
		Email: string(b.Email), Password: b.Password, InviteCode: b.InviteCode,
		Locale: requestLocale(ctx, b.Locale), Timezone: b.Timezone, CaptchaToken: b.CaptchaToken,
		IP: ipSubject(info(ctx).IP),
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateAccount202JSONResponse(accepted), nil
}

// VerifyEmail 提交邮箱验证码（AUTH-03）。已登录时只需验证码，未登录时另需邮箱。
func (s *Server) VerifyEmail(ctx context.Context, req gen.VerifyEmailRequestObject) (gen.VerifyEmailResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	if err := s.d.Accounts.VerifyEmail(ctx, principalID(ctx), emailPtr(req.Body.Email), req.Body.Code); err != nil {
		return nil, err
	}
	return gen.VerifyEmail204Response{}, nil
}

// ResendVerification 重新发送邮箱验证码（AUTH-03）。
func (s *Server) ResendVerification(ctx context.Context, req gen.ResendVerificationRequestObject) (gen.ResendVerificationResponseObject, error) {
	var email *string
	var captcha *string
	if req.Body != nil {
		email, captcha = emailPtr(req.Body.Email), req.Body.CaptchaToken
	}
	if err := s.d.Accounts.ResendVerification(ctx, principalID(ctx), email, captcha, ipSubject(info(ctx).IP)); err != nil {
		return nil, err
	}
	return gen.ResendVerification202JSONResponse(accepted), nil
}

// RequestPasswordReset 发起找回密码（AUTH-04）。无论邮箱是否存在都返回 202。
func (s *Server) RequestPasswordReset(ctx context.Context, req gen.RequestPasswordResetRequestObject) (gen.RequestPasswordResetResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	if err := s.d.Accounts.RequestReset(ctx, string(req.Body.Email), req.Body.CaptchaToken, ipSubject(info(ctx).IP)); err != nil {
		return nil, err
	}
	return gen.RequestPasswordReset202JSONResponse(accepted), nil
}

// ConfirmPasswordReset 用链接中的令牌设置新密码（AUTH-04）。
func (s *Server) ConfirmPasswordReset(ctx context.Context, req gen.ConfirmPasswordResetRequestObject) (gen.ConfirmPasswordResetResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	if err := s.d.Accounts.ConfirmReset(ctx, req.Body.Token, req.Body.NewPassword); err != nil {
		return nil, err
	}
	return gen.ConfirmPasswordReset204Response{}, nil
}
