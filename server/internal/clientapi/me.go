// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/clientapi/gen"
	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// GetMe 返回当前账号信息（spec/30 GET /v1/me）。
func (s *Server) GetMe(ctx context.Context, _ gen.GetMeRequestObject) (gen.GetMeResponseObject, error) {
	p, _ := auth.FromContext(ctx)
	row, err := sqlc.New(s.d.Pool).GetMe(ctx, p.AccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.Unauthenticated
	}
	if err != nil {
		return nil, err
	}
	me := gen.Me{
		Id:                row.ID,
		Email:             openapi_types.Email(row.Email),
		IsEmailVerified:   row.EmailVerifiedAt != nil,
		Status:            gen.MeStatus(row.Status),
		Locale:            row.Locale,
		Timezone:          row.Timezone,
		IsAutoRenew:       row.AutoRenew,
		IsMfaEnabled:      row.TotpEnabledAt != nil,
		MfaMethods:        []gen.MfaMethod{},
		EntitlementStatus: gen.EntitlementStatusNone,
		DeviceLimit:       int(row.FreeDeviceLimit),
		ReferralCode:      nullable.NewNullableWithValue(row.ReferralCode),
		CreatedAt:         row.CreatedAt,
	}
	if row.TotpEnabledAt != nil {
		me.MfaMethods = append(me.MfaMethods, gen.MfaMethodTotp, gen.MfaMethodRecoveryCode)
	}
	if row.EntitlementStatus != nil {
		me.EntitlementStatus = gen.EntitlementStatus(*row.EntitlementStatus)
		if row.PlanKind != nil && *row.PlanKind == "free" {
			me.EntitlementStatus = gen.EntitlementStatusFree
		}
		if row.EntitlementDeviceLimit != nil {
			me.DeviceLimit = int(*row.EntitlementDeviceLimit)
		}
	}
	return gen.GetMe200JSONResponse(me), nil
}
