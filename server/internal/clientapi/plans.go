// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"

	"github.com/oapi-codegen/nullable"

	"github.com/akari-project/panel/server/internal/billing/catalog"
	"github.com/akari-project/panel/server/internal/clientapi/gen"
)

// ListPlans 返回在售的非免费套餐与在售价格行（spec/11 BIL-21），不分页，至多 200 项（CONV-11）。
// 加购项价格在 M4 实现，目前为空列表（BIL-24）。
func (s *Server) ListPlans(ctx context.Context, _ gen.ListPlansRequestObject) (gen.ListPlansResponseObject, error) {
	plans, err := (&catalog.Service{Pool: s.d.Pool, Clock: s.d.Clock, Log: s.d.Log}).OnSale(ctx)
	if err != nil {
		return nil, err
	}
	resp := gen.ListPlans200JSONResponse{Items: make([]gen.Plan, len(plans)), AddonPrices: []gen.AddonPrice{}}
	for i, p := range plans {
		prices := make([]gen.PlanPrice, len(p.Prices))
		for j, pr := range p.Prices {
			days := nullable.NewNullNullable[int]()
			if pr.PeriodDays != nil {
				days = nullable.NewNullableWithValue(int(*pr.PeriodDays))
			}
			prices[j] = gen.PlanPrice{Id: pr.ID, Period: gen.PlanPricePeriod(pr.Period), PeriodDays: days,
				AmountMinor: pr.AmountMinor, Currency: pr.Currency}
		}
		speed := nullable.NewNullNullable[int]()
		if p.SpeedLimitMbps != nil {
			speed = nullable.NewNullableWithValue(int(*p.SpeedLimitMbps))
		}
		resp.Items[i] = gen.Plan{Id: p.ID, Name: p.Name, Description: p.Description, Kind: gen.PlanKind(p.Kind),
			Tier: int(p.Tier), BytesPerCycle: p.BytesPerCycle, DeviceLimit: int(p.DeviceLimit), SpeedLimitMbps: speed,
			ResetPolicy: gen.PlanResetPolicy(p.ResetPolicy), LocationCount: int(p.LocationCount), Prices: prices}
	}
	return resp, nil
}
