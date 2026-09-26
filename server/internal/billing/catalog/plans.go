// SPDX-License-Identifier: AGPL-3.0-or-later

package catalog

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/audit"
	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// Plan 是套餐及其在售价格行与线路组关联。
type Plan struct {
	ID               uuid.UUID
	Name             string
	Description      *string
	Tier             int32
	Kind             string
	Status           string
	BytesPerCycle    int64
	DeviceLimit      int32
	SpeedLimitMbps   *int32
	ResetPolicy      string
	AllowLegacyRenew bool
	Sort             int32
	Version          int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
	// ActiveEntitlementCount 是当前权益（active、over_quota、suspended）数。
	ActiveEntitlementCount int32
	GroupIDs               []uuid.UUID
	// Prices 是在售的价格行。
	Prices []Price
}

// PlanFields 是创建或修改套餐时提交的字段。
type PlanFields struct {
	Name             Opt[string]
	Description      Opt[*string]
	Tier             Opt[int]
	Kind             Opt[string]
	Status           Opt[string]
	BytesPerCycle    Opt[int64]
	DeviceLimit      Opt[int]
	SpeedLimitMbps   Opt[*int]
	ResetPolicy      Opt[string]
	AllowLegacyRenew Opt[bool]
	Sort             Opt[int32]
}

func (p PlanFields) empty() bool {
	return !p.Name.Set && !p.Description.Set && !p.Tier.Set && !p.Kind.Set && !p.Status.Set && !p.BytesPerCycle.Set &&
		!p.DeviceLimit.Set && !p.SpeedLimitMbps.Set && !p.ResetPolicy.Set && !p.AllowLegacyRenew.Set && !p.Sort.Set
}

// check 校验提交的各个字段（契约的取值范围）。
func (p PlanFields) check(f *fields) {
	if p.Name.Set {
		f.name("name", p.Name.V)
	}
	if p.Tier.Set {
		f.int32Range("tier", p.Tier.V, 0)
	}
	if p.Kind.Set {
		f.enum("kind", p.Kind.V, Kinds)
	}
	if p.Status.Set {
		f.enum("status", p.Status.V, Statuses)
	}
	if p.BytesPerCycle.Set && p.BytesPerCycle.V < 0 {
		f.add("bytes_per_cycle", "out_of_range")
	}
	if p.DeviceLimit.Set {
		f.int32Range("device_limit", p.DeviceLimit.V, 1)
	}
	if p.SpeedLimitMbps.Set && p.SpeedLimitMbps.V != nil {
		f.int32Range("speed_limit_mbps", *p.SpeedLimitMbps.V, 1)
	}
	if p.ResetPolicy.Set {
		f.enum("reset_policy", p.ResetPolicy.V, ResetPolicies)
	}
}

// planState 是套餐可修改的字段。
type planState struct {
	Name             string
	Description      *string
	Tier             int32
	Kind             string
	Status           string
	BytesPerCycle    int64
	DeviceLimit      int32
	SpeedLimitMbps   *int32
	ResetPolicy      string
	AllowLegacyRenew bool
	Sort             int32
}

func (st planState) apply(p PlanFields) planState {
	st.Name = p.Name.or(st.Name)
	st.Description = p.Description.or(st.Description)
	if p.Tier.Set {
		st.Tier = int32(p.Tier.V)
	}
	st.Kind = p.Kind.or(st.Kind)
	st.Status = p.Status.or(st.Status)
	st.BytesPerCycle = p.BytesPerCycle.or(st.BytesPerCycle)
	if p.DeviceLimit.Set {
		st.DeviceLimit = int32(p.DeviceLimit.V)
	}
	if p.SpeedLimitMbps.Set {
		st.SpeedLimitMbps = ptr32(p.SpeedLimitMbps.V)
	}
	st.ResetPolicy = p.ResetPolicy.or(st.ResetPolicy)
	st.AllowLegacyRenew = p.AllowLegacyRenew.or(st.AllowLegacyRenew)
	st.Sort = p.Sort.or(st.Sort)
	return st
}

// audit 是审计差异中记录的取值（接口字段名）。
func (st planState) audit() map[string]any {
	return map[string]any{
		"name": st.Name, "description": st.Description, "tier": st.Tier, "kind": st.Kind, "status": st.Status,
		"bytes_per_cycle": st.BytesPerCycle, "device_limit": st.DeviceLimit, "speed_limit_mbps": st.SpeedLimitMbps,
		"reset_policy": st.ResetPolicy, "is_legacy_renew_allowed": st.AllowLegacyRenew, "sort": st.Sort,
	}
}

// checkTier：tier 0 保留给免费套餐（spec/11 11.1）。
func (st planState) checkTier() error {
	if (st.Kind == "free") != (st.Tier == 0) {
		return apierr.Invalid(apierr.Field("tier", "out_of_range"))
	}
	return nil
}

func lockedState(r sqlc.LockPlanRow) planState {
	return planState{Name: r.Name, Description: r.Description, Tier: r.Tier, Kind: r.Kind, Status: r.Status,
		BytesPerCycle: r.BytesPerCycle, DeviceLimit: r.DeviceLimit, SpeedLimitMbps: r.SpeedLimitMbps,
		ResetPolicy: r.ResetPolicy, AllowLegacyRenew: r.AllowLegacyRenew, Sort: r.Sort}
}

func planFromRow(r sqlc.GetPlanRow) Plan {
	return Plan{ID: r.ID, Name: r.Name, Description: r.Description, Tier: r.Tier, Kind: r.Kind, Status: r.Status,
		BytesPerCycle: r.BytesPerCycle, DeviceLimit: r.DeviceLimit, SpeedLimitMbps: r.SpeedLimitMbps,
		ResetPolicy: r.ResetPolicy, AllowLegacyRenew: r.AllowLegacyRenew, Sort: r.Sort, Version: r.Version,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, ActiveEntitlementCount: r.ActiveEntitlementCount,
		GroupIDs: []uuid.UUID{}, Prices: []Price{}}
}

// fillPlans 读取套餐的线路组关联与在售价格行。
func fillPlans(ctx context.Context, q *sqlc.Queries, plans []Plan) error {
	if len(plans) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(plans))
	idx := make(map[uuid.UUID]int, len(plans))
	for i, p := range plans {
		ids[i] = p.ID
		idx[p.ID] = i
	}
	groups, err := q.PlanGroupIDs(ctx, ids)
	if err != nil {
		return err
	}
	for _, g := range groups {
		p := &plans[idx[g.PlanID]]
		p.GroupIDs = append(p.GroupIDs, g.GroupID)
	}
	prices, err := q.OnSalePrices(ctx, ids)
	if err != nil {
		return err
	}
	for _, r := range prices {
		p := &plans[idx[r.PlanID]]
		p.Prices = append(p.Prices, Price(r))
	}
	return nil
}

func getPlan(ctx context.Context, q *sqlc.Queries, id uuid.UUID) (Plan, error) {
	r, err := q.GetPlan(ctx, id)
	if err != nil {
		return Plan{}, notFound(err)
	}
	out := []Plan{planFromRow(r)}
	if err := fillPlans(ctx, q, out); err != nil {
		return Plan{}, err
	}
	return out[0], nil
}

// PlanKey 是套餐列表的游标键（CONV-11）。
type PlanKey struct {
	Sort int32
	ID   uuid.UUID
}

// PlanFilter 是套餐列表的筛选条件。
type PlanFilter struct {
	Status, Kind *string
	After        *PlanKey
	// Max 是返回的最大条数。
	Max int32
}

// ListPlans 按 (sort, id) 列出套餐。
func (s *Service) ListPlans(ctx context.Context, f PlanFilter) ([]Plan, error) {
	q := sqlc.New(s.Pool)
	params := sqlc.ListPlansParams{Status: f.Status, Kind: f.Kind, MaxRows: f.Max}
	if f.After != nil {
		params.CursorSort, params.CursorID = &f.After.Sort, &f.After.ID
	}
	rows, err := q.ListPlans(ctx, params)
	if err != nil {
		return nil, err
	}
	plans := make([]Plan, len(rows))
	for i, r := range rows {
		plans[i] = planFromRow(sqlc.GetPlanRow(r))
	}
	return plans, fillPlans(ctx, q, plans)
}

// GetPlan 返回套餐；不存在返回 404。
func (s *Service) GetPlan(ctx context.Context, id uuid.UUID) (Plan, error) {
	return getPlan(ctx, sqlc.New(s.Pool), id)
}

// checkGroupIDs 校验提交的线路组 ID：不重复，且都存在（不存在为 not_allowed）。
func checkGroupIDs(ctx context.Context, q *sqlc.Queries, field string, ids []uuid.UUID) (map[uuid.UUID]*int32, error) {
	var f fields
	f.uniqueIDs(field, ids)
	if err := f.err(); err != nil {
		return nil, err
	}
	rows, err := q.LocationGroupTiers(ctx, ids)
	if err != nil {
		return nil, err
	}
	if len(rows) != len(ids) {
		return nil, apierr.Invalid(apierr.Field(field, "not_allowed"))
	}
	tiers := make(map[uuid.UUID]*int32, len(rows))
	for _, r := range rows {
		tiers[r.ID] = r.MinTier
	}
	return tiers, nil
}

// freeTaken 把免费套餐唯一索引的冲突转为 400 kind taken（BIL-15）。
func freeTaken(err error) error {
	if code, constraint := pgCode(err); code == "23505" && constraint == "plans_single_free" {
		return apierr.Invalid(apierr.Field("kind", "taken"))
	}
	return err
}

// CreatePlan 创建套餐（默认 draft）并关联线路组（可以不关联）。新建时没有价格行，非免费套餐不能直接上架
// （409 invalid_state，BIL-26）；已有免费套餐时再建免费套餐返回 400 kind taken（BIL-15）。
func (s *Service) CreatePlan(ctx context.Context, in PlanFields, groupIDs []uuid.UUID) (Plan, error) {
	var f fields
	for name, set := range map[string]bool{"name": in.Name.Set, "tier": in.Tier.Set, "kind": in.Kind.Set,
		"bytes_per_cycle": in.BytesPerCycle.Set, "device_limit": in.DeviceLimit.Set} {
		if !set {
			f.add(name, "required")
		}
	}
	in.check(&f)
	f.uniqueIDs("location_group_ids", groupIDs)
	if err := f.err(); err != nil {
		return Plan{}, err
	}
	st := planState{Status: "draft", ResetPolicy: "purchase_anchor", AllowLegacyRenew: true}.apply(in)
	if err := st.checkTier(); err != nil {
		return Plan{}, err
	}
	var out Plan
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := checkGroupIDs(ctx, q, "location_group_ids", groupIDs); err != nil {
			return err
		}
		if st.Kind == "free" {
			taken, err := q.FreePlanExists(ctx, uuid.Nil)
			if err != nil {
				return err
			}
			if taken {
				return apierr.Invalid(apierr.Field("kind", "taken"))
			}
		}
		if st.Kind != "free" && st.Status == "on_sale" {
			return apierr.InvalidState
		}
		id, err := q.InsertPlan(ctx, sqlc.InsertPlanParams{Name: st.Name, Description: st.Description, Tier: st.Tier,
			Kind: st.Kind, Status: st.Status, BytesPerCycle: st.BytesPerCycle, DeviceLimit: st.DeviceLimit,
			SpeedLimitMbps: st.SpeedLimitMbps, ResetPolicy: st.ResetPolicy, AllowLegacyRenew: st.AllowLegacyRenew, Sort: st.Sort})
		if err != nil {
			return freeTaken(err)
		}
		for _, g := range groupIDs {
			if _, err := q.InsertPlanGroup(ctx, sqlc.InsertPlanGroupParams{PlanID: id, GroupID: g}); err != nil {
				if code, _ := pgCode(err); code == "23503" { // 线路组已被并发删除
					return apierr.Invalid(apierr.Field("location_group_ids", "not_allowed"))
				}
				return err
			}
		}
		// 新套餐没有权益，关联线路组不改变任何访问关系，不写 plan.access_changed。
		values := st.audit()
		values["location_group_ids"] = groupIDs
		if err := audit.Record(ctx, q, audit.Entry{Action: "plan.create", TargetType: "plan", TargetID: id.String(),
			Diff: audit.Values(values)}); err != nil {
			return err
		}
		out, err = getPlan(ctx, q, id)
		return err
	})
	return out, err
}

func lockPlan(ctx context.Context, q *sqlc.Queries, id uuid.UUID, ifMatch string) (sqlc.LockPlanRow, error) {
	r, err := q.LockPlan(ctx, id)
	if err != nil {
		return r, notFound(err)
	}
	return r, checkIfMatch(ifMatch, ETag(r.Version))
}

// UpdatePlan 修改套餐（BIL-02：不影响已有权益的快照）。规则（BIL-26）：
//   - kind 只能在没有价格行与权益、且未被设置 free_plan_id 引用时修改，否则 409 invalid_state；
//     kind 与 tier 不一致为 400 tier out_of_range；
//   - 按修改后的状态检查：非免费套餐处于 on_sale 时必须至少有一个在售价格行，否则 409 invalid_state；
//     免费套餐的状态不受限制（BIL-15）；有权益的套餐不能改回 draft；
//   - tier 变化写 plan.access_changed（ACS-05）；状态变化不写事件。
//
// 没有实际变化时不修改版本、不写审计。
func (s *Service) UpdatePlan(ctx context.Context, id uuid.UUID, ifMatch string, in PlanFields) (Plan, error) {
	var f fields
	if in.empty() {
		return Plan{}, apierr.Invalid()
	}
	in.check(&f)
	if err := f.err(); err != nil {
		return Plan{}, err
	}
	var out Plan
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		cur, err := lockPlan(ctx, q, id, ifMatch)
		if err != nil {
			return err
		}
		before := lockedState(cur)
		after := before.apply(in)
		if err := after.checkTier(); err != nil {
			return err
		}
		changes := audit.Changes(before.audit(), after.audit())
		if len(changes) == 0 {
			out, err = getPlan(ctx, q, id)
			return err
		}
		usage, err := q.PlanUsage(ctx, id)
		if err != nil {
			return err
		}
		if after.Kind != before.Kind {
			if usage.HasPrices || usage.HasEntitlements {
				return apierr.InvalidState
			}
			free, err := s.freePlanID(ctx, q)
			if err != nil {
				return err
			}
			if free != nil && *free == id {
				return apierr.InvalidState
			}
			if after.Kind == "free" {
				taken, err := q.FreePlanExists(ctx, id)
				if err != nil {
					return err
				}
				if taken {
					return apierr.Invalid(apierr.Field("kind", "taken"))
				}
			}
		}
		if after.Kind != "free" && after.Status == "on_sale" && usage.OnSalePrices == 0 {
			return apierr.InvalidState
		}
		if after.Status == "draft" && before.Status != "draft" && usage.HasEntitlements {
			return apierr.InvalidState
		}
		if err := q.UpdatePlan(ctx, sqlc.UpdatePlanParams{ID: id, Name: after.Name, Description: after.Description,
			Tier: after.Tier, Kind: after.Kind, Status: after.Status, BytesPerCycle: after.BytesPerCycle,
			DeviceLimit: after.DeviceLimit, SpeedLimitMbps: after.SpeedLimitMbps, ResetPolicy: after.ResetPolicy,
			AllowLegacyRenew: after.AllowLegacyRenew, Sort: after.Sort}); err != nil {
			return freeTaken(err)
		}
		if after.Tier != before.Tier {
			if err := emit(ctx, q, "plan.access_changed", map[string]any{"plan_id": id}); err != nil {
				return err
			}
		}
		if err := audit.Record(ctx, q, audit.Entry{Action: "plan.update", TargetType: "plan", TargetID: id.String(),
			Diff: changes}); err != nil {
			return err
		}
		out, err = getPlan(ctx, q, id)
		return err
	})
	return out, err
}

// DeletePlan 删除没有价格行与权益、且未被设置 free_plan_id 引用的套餐（BIL-26：否则 409 invalid_state，
// 应改为 archived）。线路组关联随之删除。
func (s *Service) DeletePlan(ctx context.Context, id uuid.UUID, ifMatch string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		cur, err := lockPlan(ctx, q, id, ifMatch)
		if err != nil {
			return err
		}
		usage, err := q.PlanUsage(ctx, id)
		if err != nil {
			return err
		}
		if usage.HasPrices || usage.HasEntitlements {
			return apierr.InvalidState
		}
		free, err := s.freePlanID(ctx, q)
		if err != nil {
			return err
		}
		if free != nil && *free == id {
			return apierr.InvalidState
		}
		if err := q.DeletePlan(ctx, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: "plan.delete", TargetType: "plan", TargetID: id.String(),
			Diff: audit.Values(map[string]any{"name": cur.Name, "kind": cur.Kind, "tier": cur.Tier})})
	})
}

// AddPlanGroup 为套餐关联线路组（BIL-04）。已关联时返回当前套餐，不修改版本。线路组不存在返回 404。
func (s *Service) AddPlanGroup(ctx context.Context, id, group uuid.UUID, ifMatch string) (Plan, error) {
	var out Plan
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := lockPlan(ctx, q, id, ifMatch); err != nil {
			return err
		}
		n, err := q.InsertPlanGroup(ctx, sqlc.InsertPlanGroupParams{PlanID: id, GroupID: group})
		if code, _ := pgCode(err); code == "23503" {
			return apierr.NotFound
		}
		if err != nil {
			return err
		}
		if n > 0 {
			if err := linkChanged(ctx, q, id, group, "plan_location_group.create", ""); err != nil {
				return err
			}
		}
		out, err = getPlan(ctx, q, id)
		return err
	})
	return out, err
}

// RemovePlanGroup 取消套餐与线路组的关联（BIL-04，敏感操作，原因由调用方校验）。未关联返回 404。
func (s *Service) RemovePlanGroup(ctx context.Context, id, group uuid.UUID, ifMatch, reason string) (Plan, error) {
	var out Plan
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := lockPlan(ctx, q, id, ifMatch); err != nil {
			return err
		}
		n, err := q.DeletePlanGroup(ctx, sqlc.DeletePlanGroupParams{PlanID: id, GroupID: group})
		if err != nil {
			return err
		}
		if n == 0 {
			return apierr.NotFound
		}
		if err := linkChanged(ctx, q, id, group, "plan_location_group.delete", reason); err != nil {
			return err
		}
		out, err = getPlan(ctx, q, id)
		return err
	})
	return out, err
}

// linkChanged 在线路组关联增删后：套餐的版本加 1（线路组的 plan_ids 是派生字段，版本不变，CON-05），
// 写 plan.access_changed 与审计（目标为套餐）。
func linkChanged(ctx context.Context, q *sqlc.Queries, plan, group uuid.UUID, action, reason string) error {
	if err := q.BumpPlanVersion(ctx, plan); err != nil {
		return err
	}
	if err := emit(ctx, q, "plan.access_changed", map[string]any{"plan_id": plan}); err != nil {
		return err
	}
	return audit.Record(ctx, q, audit.Entry{Action: action, TargetType: "plan", TargetID: plan.String(),
		Diff: audit.Values(map[string]any{"location_group_id": group}), Reason: reason})
}
