// SPDX-License-Identifier: AGPL-3.0-or-later

package catalog

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// Impact 是影响预览（spec/31 CON-07，spec/32 UI-03）：只计算，不产生副作用。
type Impact struct {
	AccountCount int32
	HostCount    int32
	ComputedAt   time.Time
}

// PlanProposal 是套餐的拟议变更。
type PlanProposal struct {
	// GroupIDs 为拟设置的线路组集合；nil 表示不变。
	GroupIDs      *[]uuid.UUID
	Tier          *int
	Status        *string
	RolloutFields []string
}

// eligible 报告等级为 tier 的套餐能否访问 min_tier 的线路组（ACS-05）。
func eligible(tier int32, minTier *int32) bool { return minTier == nil || tier >= *minTier }

// PreviewPlan 计算套餐拟议变更的影响：
//   - 访问关系变化的线路组：关联增删，或 tier 变化使可访问性改变的线路组；节点数为这些线路组的成员节点去重；
//   - 账号数：访问关系、状态有变化或选择了“应用到现有用户”的字段时，为持有该套餐当前权益的账号数，否则为 0。
func (s *Service) PreviewPlan(ctx context.Context, id uuid.UUID, in PlanProposal) (Impact, error) {
	var f fields
	if in.GroupIDs == nil && in.Tier == nil && in.Status == nil && len(in.RolloutFields) == 0 {
		return Impact{}, apierr.Invalid()
	}
	if in.Tier != nil {
		f.int32Range("tier", *in.Tier, 0)
	}
	if in.Status != nil {
		f.enum("status", *in.Status, Statuses)
	}
	for _, r := range in.RolloutFields {
		f.enum("rollout_fields", r, RolloutFields)
	}
	if in.GroupIDs != nil {
		f.uniqueIDs("location_group_ids", *in.GroupIDs)
	}
	if err := f.err(); err != nil {
		return Impact{}, err
	}
	q := sqlc.New(s.Pool)
	p, err := getPlan(ctx, q, id)
	if err != nil {
		return Impact{}, err
	}
	proposed := p.GroupIDs
	if in.GroupIDs != nil {
		proposed = *in.GroupIDs
	}
	tier := p.Tier
	if in.Tier != nil {
		tier = int32(*in.Tier)
	}
	all := slices.Concat(p.GroupIDs, proposed)
	slices.SortFunc(all, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	all = slices.Compact(all)
	rows, err := q.LocationGroupTiers(ctx, all)
	if err != nil {
		return Impact{}, err
	}
	tiers := make(map[uuid.UUID]*int32, len(rows))
	for _, r := range rows {
		tiers[r.ID] = r.MinTier
	}
	if in.GroupIDs != nil {
		for _, g := range *in.GroupIDs {
			if _, ok := tiers[g]; !ok {
				return Impact{}, apierr.Invalid(apierr.Field("location_group_ids", "not_allowed"))
			}
		}
	}
	var changed []uuid.UUID
	for _, g := range all {
		before := slices.Contains(p.GroupIDs, g) && eligible(p.Tier, tiers[g])
		after := slices.Contains(proposed, g) && eligible(tier, tiers[g])
		if before != after {
			changed = append(changed, g)
		}
	}
	out := Impact{ComputedAt: s.now()}
	statusChanged := in.Status != nil && *in.Status != p.Status
	if len(changed) > 0 || statusChanged || len(in.RolloutFields) > 0 {
		if out.AccountCount, err = q.CountPlanHolders(ctx, id); err != nil {
			return Impact{}, err
		}
	}
	if len(changed) > 0 {
		if out.HostCount, err = q.CountGroupHosts(ctx, changed); err != nil {
			return Impact{}, err
		}
	}
	return out, nil
}

// GroupProposal 是线路组的拟议变更。
type GroupProposal struct {
	MinTier     Opt[*int]
	AddHosts    []uuid.UUID
	RemoveHosts []uuid.UUID
	IsDeletion  bool
}

// PreviewGroup 计算线路组拟议变更的影响：
//   - 账号数：只改 min_tier 时，为可访问性因此改变的账号；增删节点或删除时，为在变更前后任一 min_tier 下
//     可访问该线路组的账号；
//   - 节点数：访问关系有变化（min_tier 影响了账号或删除）时为全部成员节点，加上增删的节点，去重。
func (s *Service) PreviewGroup(ctx context.Context, id uuid.UUID, in GroupProposal) (Impact, error) {
	if !in.MinTier.Set && len(in.AddHosts) == 0 && len(in.RemoveHosts) == 0 && !in.IsDeletion {
		return Impact{}, apierr.Invalid()
	}
	var f fields
	if in.MinTier.Set && in.MinTier.V != nil {
		f.int32Range("min_tier", *in.MinTier.V, 0)
	}
	f.uniqueIDs("add_host_ids", in.AddHosts)
	f.uniqueIDs("remove_host_ids", in.RemoveHosts)
	if err := f.err(); err != nil {
		return Impact{}, err
	}
	q := sqlc.New(s.Pool)
	g, err := q.GetLocationGroup(ctx, id)
	if err != nil {
		return Impact{}, notFound(err)
	}
	for field, ids := range map[string][]uuid.UUID{"add_host_ids": in.AddHosts, "remove_host_ids": in.RemoveHosts} {
		if len(ids) == 0 {
			continue
		}
		found, err := q.ExistingNodeIDs(ctx, ids)
		if err != nil {
			return Impact{}, err
		}
		if len(found) != len(ids) {
			return Impact{}, apierr.Invalid(apierr.Field(field, "not_allowed"))
		}
	}
	newMin := g.MinTier
	if in.MinTier.Set {
		newMin = ptr32(in.MinTier.V)
	}
	hostsChange := len(in.AddHosts) > 0 || len(in.RemoveHosts) > 0 || in.IsDeletion
	out := Impact{ComputedAt: s.now()}
	if out.AccountCount, err = q.CountGroupHolders(ctx, sqlc.CountGroupHoldersParams{GroupID: id, OnlyChanged: !hostsChange,
		OldMin: g.MinTier, NewMin: newMin}); err != nil {
		return Impact{}, err
	}
	hosts := map[uuid.UUID]bool{}
	if out.AccountCount > 0 || in.IsDeletion {
		members, err := q.GroupMemberIDs(ctx, id)
		if err != nil {
			return Impact{}, err
		}
		for _, m := range members {
			hosts[m] = true
		}
	}
	for _, h := range slices.Concat(in.AddHosts, in.RemoveHosts) {
		hosts[h] = true
	}
	out.HostCount = int32(len(hosts))
	return out, nil
}
