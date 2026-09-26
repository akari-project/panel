// SPDX-License-Identifier: AGPL-3.0-or-later

package catalog

import (
	"context"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// PublicPlan 是客户端接口列出的在售套餐（spec/30 listPlans）。
type PublicPlan struct {
	ID             uuid.UUID
	Name           string
	Description    *string
	Tier           int32
	Kind           string
	BytesPerCycle  int64
	DeviceLimit    int32
	SpeedLimitMbps *int32
	ResetPolicy    string
	// LocationCount 是可访问的地区数：关联且满足 min_tier 的线路组中节点的 region_code 去重。
	LocationCount int32
	// Prices 是在售的价格行。
	Prices []Price
}

// OnSale 返回在售的非免费套餐（BIL-21），按 (sort, id)，至多 MaxPublicPlans 项。
func (s *Service) OnSale(ctx context.Context) ([]PublicPlan, error) {
	q := sqlc.New(s.Pool)
	rows, err := q.ListOnSalePlans(ctx, MaxPublicPlans)
	if err != nil {
		return nil, err
	}
	out := make([]PublicPlan, len(rows))
	ids := make([]uuid.UUID, len(rows))
	idx := make(map[uuid.UUID]int, len(rows))
	for i, r := range rows {
		out[i] = PublicPlan{ID: r.ID, Name: r.Name, Description: r.Description, Tier: r.Tier, Kind: r.Kind,
			BytesPerCycle: r.BytesPerCycle, DeviceLimit: r.DeviceLimit, SpeedLimitMbps: r.SpeedLimitMbps,
			ResetPolicy: r.ResetPolicy, Prices: []Price{}}
		ids[i] = r.ID
		idx[r.ID] = i
	}
	if len(rows) == 0 {
		return out, nil
	}
	prices, err := q.OnSalePrices(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, p := range prices {
		pp := &out[idx[p.PlanID]]
		pp.Prices = append(pp.Prices, Price(p))
	}
	regions, err := q.PlanRegionCounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range regions {
		out[idx[r.PlanID]].LocationCount = r.Regions
	}
	return out, nil
}
