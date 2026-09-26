// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"context"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/billing/catalog"
	"github.com/akari-project/panel/server/internal/consoleapi/gen"
)

// 套餐、价格行与线路组的规则在 internal/billing/catalog（spec/11）；本文件只做契约类型的转换。

func opt[T any](p *T) catalog.Opt[T] {
	if p == nil {
		return catalog.Opt[T]{}
	}
	return catalog.Some(*p)
}

func optEnum[E ~string](p *E) catalog.Opt[string] {
	if p == nil {
		return catalog.Opt[string]{}
	}
	return catalog.Some(string(*p))
}

// optNullable 把可空字段转为可选字段：未提供为未设置，null 为 nil。
func optNullable[T any](n nullable.Nullable[T]) catalog.Opt[*T] {
	if !n.IsSpecified() {
		return catalog.Opt[*T]{}
	}
	if n.IsNull() {
		return catalog.Some[*T](nil)
	}
	v := n.MustGet()
	return catalog.Some(&v)
}

func nullableOf[T, U any](p *T, conv func(T) U) nullable.Nullable[U] {
	if p == nil {
		return nullable.NewNullNullable[U]()
	}
	return nullable.NewNullableWithValue(conv(*p))
}

func toInt(v int32) int { return int(v) }

func ifMatch(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func planAPI(p catalog.Plan) gen.Plan {
	prices := make([]gen.PlanPrice, len(p.Prices))
	for i, pr := range p.Prices {
		prices[i] = priceAPI(pr)
	}
	return gen.Plan{
		Id: p.ID, Name: p.Name, Description: nullableOf(p.Description, func(s string) string { return s }),
		Tier: int(p.Tier), Kind: gen.PlanKind(p.Kind), Status: gen.PlanStatus(p.Status), BytesPerCycle: p.BytesPerCycle,
		DeviceLimit: int(p.DeviceLimit), SpeedLimitMbps: nullableOf(p.SpeedLimitMbps, toInt),
		ResetPolicy: gen.PlanResetPolicy(p.ResetPolicy), IsLegacyRenewAllowed: p.AllowLegacyRenew, Sort: p.Sort,
		LocationGroupIds: p.GroupIDs, Prices: prices, ActiveEntitlementCount: p.ActiveEntitlementCount,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func priceAPI(p catalog.Price) gen.PlanPrice {
	return gen.PlanPrice{Id: p.ID, PlanId: p.PlanID, Period: gen.PlanPricePeriod(p.Period),
		PeriodDays: nullableOf(p.PeriodDays, func(v int32) int32 { return v }), AmountMinor: p.AmountMinor,
		Currency: p.Currency, IsOnSale: p.OnSale, CreatedAt: p.CreatedAt}
}

func planETag(p catalog.Plan) *string {
	e := catalog.ETag(p.Version)
	return &e
}

// ListPlans 按 (sort, id) 分页列出套餐（CONV-11）。
func (s *Server) ListPlans(ctx context.Context, req gen.ListPlansRequestObject) (gen.ListPlansResponseObject, error) {
	limit, err := pageLimit(req.Params.Limit)
	if err != nil {
		return nil, err
	}
	var cur struct {
		Sort int32     `json:"sort"`
		ID   uuid.UUID `json:"id"`
	}
	f := catalog.PlanFilter{Max: limit + 1}
	if ok, err := decodeCursor(req.Params.Cursor, &cur); err != nil {
		return nil, err
	} else if ok {
		f.After = &catalog.PlanKey{Sort: cur.Sort, ID: cur.ID}
	}
	if v := req.Params.Status; v != nil {
		if !v.Valid() {
			return nil, apierr.Invalid(apierr.Field("status", "invalid_format"))
		}
		f.Status = (*string)(v)
	}
	if v := req.Params.Kind; v != nil {
		if !v.Valid() {
			return nil, apierr.Invalid(apierr.Field("kind", "invalid_format"))
		}
		f.Kind = (*string)(v)
	}
	plans, err := s.catalog.ListPlans(ctx, f)
	if err != nil {
		return nil, err
	}
	var next *string
	if len(plans) > int(limit) {
		plans = plans[:limit]
		last := plans[len(plans)-1]
		next = encodeCursor(map[string]any{"sort": last.Sort, "id": last.ID})
	}
	resp := gen.ListPlans200JSONResponse{Items: make([]gen.Plan, len(plans)), NextCursor: nullableString(next)}
	for i, p := range plans {
		resp.Items[i] = planAPI(p)
	}
	return resp, nil
}

// CreatePlan 创建套餐（默认 draft）。
func (s *Server) CreatePlan(ctx context.Context, req gen.CreatePlanRequestObject) (gen.CreatePlanResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	b := req.Body
	in := catalog.PlanFields{
		Name: catalog.Some(b.Name), Description: optNullable(b.Description), Tier: catalog.Some(b.Tier),
		Kind: catalog.Some(string(b.Kind)), Status: optEnum(b.Status), BytesPerCycle: catalog.Some(b.BytesPerCycle),
		DeviceLimit: catalog.Some(b.DeviceLimit), SpeedLimitMbps: optNullable(b.SpeedLimitMbps),
		ResetPolicy: optEnum(b.ResetPolicy), AllowLegacyRenew: opt(b.IsLegacyRenewAllowed), Sort: opt(b.Sort),
	}
	var groups []uuid.UUID
	if b.LocationGroupIds != nil {
		groups = *b.LocationGroupIds
	}
	p, err := s.catalog.CreatePlan(ctx, in, groups)
	if err != nil {
		return nil, err
	}
	return gen.CreatePlan201JSONResponse{Body: planAPI(p), Headers: gen.CreatePlan201ResponseHeaders{ETag: planETag(p)}}, nil
}

// GetPlan 返回套餐详情，带强 ETag，支持 If-None-Match（CONV-13）。
func (s *Server) GetPlan(ctx context.Context, req gen.GetPlanRequestObject) (gen.GetPlanResponseObject, error) {
	p, err := s.catalog.GetPlan(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	etag := planETag(p)
	if req.Params.IfNoneMatch != nil && *req.Params.IfNoneMatch == *etag {
		return gen.GetPlan304Response{Headers: gen.NotModifiedResponseHeaders{ETag: etag}}, nil
	}
	return gen.GetPlan200JSONResponse{Body: planAPI(p), Headers: gen.GetPlan200ResponseHeaders{ETag: etag}}, nil
}

// UpdatePlan 修改套餐。必须携带 If-Match（CONV-28）。
func (s *Server) UpdatePlan(ctx context.Context, req gen.UpdatePlanRequestObject) (gen.UpdatePlanResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	b := req.Body
	in := catalog.PlanFields{
		Name: opt(b.Name), Description: optNullable(b.Description), Tier: opt(b.Tier), Kind: optEnum(b.Kind),
		Status: optEnum(b.Status), BytesPerCycle: opt(b.BytesPerCycle), DeviceLimit: opt(b.DeviceLimit),
		SpeedLimitMbps: optNullable(b.SpeedLimitMbps), ResetPolicy: optEnum(b.ResetPolicy),
		AllowLegacyRenew: opt(b.IsLegacyRenewAllowed), Sort: opt(b.Sort),
	}
	p, err := s.catalog.UpdatePlan(ctx, req.Id, ifMatch(req.Params.IfMatch), in)
	if err != nil {
		return nil, err
	}
	return gen.UpdatePlan200JSONResponse{Body: planAPI(p), Headers: gen.UpdatePlan200ResponseHeaders{ETag: planETag(p)}}, nil
}

// DeletePlan 删除没有价格行与权益的套餐。必须携带 If-Match（CONV-28）。
func (s *Server) DeletePlan(ctx context.Context, req gen.DeletePlanRequestObject) (gen.DeletePlanResponseObject, error) {
	if err := s.catalog.DeletePlan(ctx, req.Id, ifMatch(req.Params.IfMatch)); err != nil {
		return nil, err
	}
	return gen.DeletePlan204Response{}, nil
}

// AddPlanLocationGroup 为套餐关联线路组（BIL-04）；If-Match 为套餐的 ETag。
func (s *Server) AddPlanLocationGroup(ctx context.Context, req gen.AddPlanLocationGroupRequestObject) (gen.AddPlanLocationGroupResponseObject, error) {
	p, err := s.catalog.AddPlanGroup(ctx, req.Id, req.GroupId, ifMatch(req.Params.IfMatch))
	if err != nil {
		return nil, err
	}
	return gen.AddPlanLocationGroup200JSONResponse{Body: planAPI(p),
		Headers: gen.AddPlanLocationGroup200ResponseHeaders{ETag: planETag(p)}}, nil
}

// RemovePlanLocationGroup 取消关联（BIL-04，敏感操作 AUTH-19）：先校验 Audit-Reason（400），再校验 Mfa-Assertion（401）。
func (s *Server) RemovePlanLocationGroup(ctx context.Context, req gen.RemovePlanLocationGroupRequestObject) (gen.RemovePlanLocationGroupResponseObject, error) {
	reason, err := headerReason(req.Params.AuditReason)
	if err != nil {
		return nil, err
	}
	if err := s.requireStepUp(ctx); err != nil {
		return nil, err
	}
	p, err := s.catalog.RemovePlanGroup(ctx, req.Id, req.GroupId, ifMatch(req.Params.IfMatch), reason)
	if err != nil {
		return nil, err
	}
	return gen.RemovePlanLocationGroup200JSONResponse{Body: planAPI(p),
		Headers: gen.RemovePlanLocationGroup200ResponseHeaders{ETag: planETag(p)}}, nil
}

// ListPlanPrices 按创建顺序倒序分页列出价格行（CONV-11）。
func (s *Server) ListPlanPrices(ctx context.Context, req gen.ListPlanPricesRequestObject) (gen.ListPlanPricesResponseObject, error) {
	limit, err := pageLimit(req.Params.Limit)
	if err != nil {
		return nil, err
	}
	var cur struct {
		ID uuid.UUID `json:"id"`
	}
	f := catalog.PriceFilter{OnSale: req.Params.IsOnSale, Max: limit + 1}
	if ok, err := decodeCursor(req.Params.Cursor, &cur); err != nil {
		return nil, err
	} else if ok {
		f.Before = &cur.ID
	}
	prices, err := s.catalog.ListPrices(ctx, req.Id, f)
	if err != nil {
		return nil, err
	}
	var next *string
	if len(prices) > int(limit) {
		prices = prices[:limit]
		next = encodeCursor(map[string]any{"id": prices[len(prices)-1].ID})
	}
	resp := gen.ListPlanPrices200JSONResponse{Items: make([]gen.PlanPrice, len(prices)), NextCursor: nullableString(next)}
	for i, p := range prices {
		resp.Items[i] = priceAPI(p)
	}
	return resp, nil
}

// CreatePlanPrice 新建价格行（BIL-01），同一周期的旧在售行在同一事务中停售。
func (s *Server) CreatePlanPrice(ctx context.Context, req gen.CreatePlanPriceRequestObject) (gen.CreatePlanPriceResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	in := catalog.PriceFields{Period: string(req.Body.Period), AmountMinor: req.Body.AmountMinor, Currency: req.Body.Currency}
	if d := optNullable(req.Body.PeriodDays); d.Set {
		in.PeriodDays = d.V
	}
	p, err := s.catalog.CreatePrice(ctx, req.Id, in)
	if err != nil {
		return nil, err
	}
	etag := catalog.PriceETag(p)
	return gen.CreatePlanPrice201JSONResponse{Body: priceAPI(p), Headers: gen.CreatePlanPrice201ResponseHeaders{ETag: &etag}}, nil
}

// GetPlanPrice 返回价格行，带强 ETag，支持 If-None-Match（CONV-13）。
func (s *Server) GetPlanPrice(ctx context.Context, req gen.GetPlanPriceRequestObject) (gen.GetPlanPriceResponseObject, error) {
	p, err := s.catalog.GetPrice(ctx, req.Id, req.PriceId)
	if err != nil {
		return nil, err
	}
	etag := catalog.PriceETag(p)
	if req.Params.IfNoneMatch != nil && *req.Params.IfNoneMatch == etag {
		return gen.GetPlanPrice304Response{Headers: gen.NotModifiedResponseHeaders{ETag: &etag}}, nil
	}
	return gen.GetPlanPrice200JSONResponse{Body: priceAPI(p), Headers: gen.GetPlanPrice200ResponseHeaders{ETag: &etag}}, nil
}

// DiscontinuePlanPrice 停售价格行（BIL-01）：请求体只接受 {"is_on_sale": false}。必须携带 If-Match（CONV-28）。
func (s *Server) DiscontinuePlanPrice(ctx context.Context, req gen.DiscontinuePlanPriceRequestObject) (gen.DiscontinuePlanPriceResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	if v, ok := req.Body.IsOnSale.(bool); !ok || v {
		return nil, apierr.Invalid(apierr.Field("is_on_sale", "not_allowed"))
	}
	p, err := s.catalog.DiscontinuePrice(ctx, req.Id, req.PriceId, ifMatch(req.Params.IfMatch))
	if err != nil {
		return nil, err
	}
	etag := catalog.PriceETag(p)
	return gen.DiscontinuePlanPrice200JSONResponse{Body: priceAPI(p), Headers: gen.DiscontinuePlanPrice200ResponseHeaders{ETag: &etag}}, nil
}

func impactAPI(i catalog.Impact) gen.ImpactPreview {
	return gen.ImpactPreview{AffectedAccountCount: i.AccountCount, AffectedHostCount: i.HostCount, ComputedAt: i.ComputedAt}
}

// PreviewPlanImpact 计算套餐拟议变更的影响（CON-07），不产生副作用。
func (s *Server) PreviewPlanImpact(ctx context.Context, req gen.PreviewPlanImpactRequestObject) (gen.PreviewPlanImpactResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	in := catalog.PlanProposal{GroupIDs: req.Body.LocationGroupIds, Tier: req.Body.Tier, Status: (*string)(req.Body.Status)}
	if req.Body.RolloutFields != nil {
		for _, f := range *req.Body.RolloutFields {
			in.RolloutFields = append(in.RolloutFields, string(f))
		}
	}
	out, err := s.catalog.PreviewPlan(ctx, req.Id, in)
	if err != nil {
		return nil, err
	}
	return gen.PreviewPlanImpact200JSONResponse(impactAPI(out)), nil
}
