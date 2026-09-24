// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/clientapi/gen"
	"github.com/akari-project/panel/server/internal/session"
)

// StartTotpEnrollment 开始绑定 TOTP（AUTH-11）。响应含密钥，不接受 Idempotency-Key（CONV-12）。
func (s *Server) StartTotpEnrollment(ctx context.Context, _ gen.StartTotpEnrollmentRequestObject) (gen.StartTotpEnrollmentResponseObject, error) {
	p, _ := auth.FromContext(ctx)
	e, err := s.d.MFA.StartEnrollment(ctx, p.AccountID)
	if err != nil {
		return nil, err
	}
	return gen.StartTotpEnrollment201JSONResponse{Secret: e.Secret, OtpauthUri: e.URI, ExpiresAt: e.ExpiresAt}, nil
}

// ActivateTotp 提交一次验证码确认绑定，返回 10 个恢复码（AUTH-11）。
func (s *Server) ActivateTotp(ctx context.Context, req gen.ActivateTotpRequestObject) (gen.ActivateTotpResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	p, _ := auth.FromContext(ctx)
	codes, err := s.d.MFA.Activate(ctx, p.AccountID, req.Body.TotpCode)
	if err != nil {
		return nil, err
	}
	return gen.ActivateTotp200JSONResponse{RecoveryCodes: codes}, nil
}

// DisableTotp 停用 TOTP（需要重新验证，AUTH-23；管理员不能停用，AUTH-12）。
func (s *Server) DisableTotp(ctx context.Context, _ gen.DisableTotpRequestObject) (gen.DisableTotpResponseObject, error) {
	p, _ := auth.FromContext(ctx)
	if err := s.d.Sessions.RequireRecentAuth(ctx, p); err != nil {
		return nil, err
	}
	if err := s.d.MFA.Disable(ctx, p.AccountID); err != nil {
		return nil, err
	}
	return gen.DisableTotp204Response{}, nil
}

// RegenerateRecoveryCodes 重新生成恢复码（需要重新验证，AUTH-23）。
func (s *Server) RegenerateRecoveryCodes(ctx context.Context, _ gen.RegenerateRecoveryCodesRequestObject) (gen.RegenerateRecoveryCodesResponseObject, error) {
	p, _ := auth.FromContext(ctx)
	if err := s.d.Sessions.RequireRecentAuth(ctx, p); err != nil {
		return nil, err
	}
	codes, err := s.d.MFA.RegenerateRecoveryCodes(ctx, p.AccountID)
	if err != nil {
		return nil, err
	}
	return gen.RegenerateRecoveryCodes200JSONResponse{RecoveryCodes: codes}, nil
}

// Reauthenticate 重新验证（AUTH-23）：密码、TOTP 码或恢复码之一。Passkey 在 M4 实现。
func (s *Server) Reauthenticate(ctx context.Context, _ gen.ReauthenticateRequestObject) (gen.ReauthenticateResponseObject, error) {
	var body struct {
		Password          string          `json:"password"`
		TotpCode          string          `json:"totp_code"`
		RecoveryCode      string          `json:"recovery_code"`
		WebauthnAssertion json.RawMessage `json:"webauthn_assertion"`
		ChallengeID       *string         `json:"challenge_id"`
	}
	dec := json.NewDecoder(bytes.NewReader(info(ctx).Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return nil, apierr.Invalid()
	}
	if len(body.WebauthnAssertion) > 0 {
		return nil, apierr.Invalid(apierr.Field("webauthn_assertion", "not_allowed"))
	}
	p, _ := auth.FromContext(ctx)
	exp, err := s.d.Sessions.Reauthenticate(ctx, p, session.Reauth{
		Password: body.Password, TOTPCode: body.TotpCode, RecoveryCode: body.RecoveryCode, IP: ipSubject(info(ctx).IP),
	})
	if err != nil {
		return nil, err
	}
	return gen.Reauthenticate200JSONResponse{ExpiresAt: exp}, nil
}

// ChangePassword 修改密码（需要重新验证，AUTH-23），成功后吊销除当前会话外的全部会话。
func (s *Server) ChangePassword(ctx context.Context, req gen.ChangePasswordRequestObject) (gen.ChangePasswordResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	p, _ := auth.FromContext(ctx)
	if err := s.d.Sessions.ChangePassword(ctx, p, req.Body.NewPassword, s.d.Accounts.Password); err != nil {
		return nil, err
	}
	return gen.ChangePassword204Response{}, nil
}
