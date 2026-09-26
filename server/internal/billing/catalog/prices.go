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

// Price 是价格行（BIL-01）。
type Price struct {
	ID          uuid.UUID
	PlanID      uuid.UUID
	Period      string
	PeriodDays  *int32
	AmountMinor int64
	Currency    string
	OnSale      bool
	CreatedAt   time.Time
}

// PriceFields 是新建价格行时提交的字段。
type PriceFields struct {
	Period      string
	PeriodDays  *int
	AmountMinor int64
	Currency    string
}

// PriceFilter 是价格行列表的筛选条件，按创建顺序倒序。
type PriceFilter struct {
	OnSale *bool
	Before *uuid.UUID
	Max    int32
}

// ListPrices 列出套餐的价格行；套餐不存在返回 404。
func (s *Service) ListPrices(ctx context.Context, plan uuid.UUID, f PriceFilter) ([]Price, error) {
	q := sqlc.New(s.Pool)
	if _, err := q.GetPlan(ctx, plan); err != nil {
		return nil, notFound(err)
	}
	rows, err := q.ListPlanPrices(ctx, sqlc.ListPlanPricesParams{PlanID: plan, OnSale: f.OnSale, Before: f.Before, MaxRows: f.Max})
	if err != nil {
		return nil, err
	}
	out := make([]Price, len(rows))
	for i, r := range rows {
		out[i] = Price(r)
	}
	return out, nil
}

// GetPrice 返回价格行；不存在或不属于该套餐返回 404。
func (s *Service) GetPrice(ctx context.Context, plan, id uuid.UUID) (Price, error) {
	r, err := sqlc.New(s.Pool).GetPlanPrice(ctx, sqlc.GetPlanPriceParams{ID: id, PlanID: plan})
	if err != nil {
		return Price{}, notFound(err)
	}
	return Price(r), nil
}

// CreatePrice 新建价格行（BIL-01：改价就是新建一行）。同一周期已有在售行时，旧行在同一事务中停售。
//   - 免费套餐不设价格行：409 invalid_state；
//   - 周期必须与套餐类型一致（recurring 用 month、quarter、half_year、year，one_time 用 one_time），
//     period_days 只用于 one_time：否则 400 not_allowed；
//   - 币种必须等于站点结算货币（CONV-08）：否则 400 currency not_allowed。
//
// 套餐的版本加 1；审计 plan_price.create 的差异中 discontinued_price_id 为被停售的旧行。
func (s *Service) CreatePrice(ctx context.Context, plan uuid.UUID, in PriceFields) (Price, error) {
	var f fields
	f.enum("period", in.Period, Periods)
	if in.PeriodDays != nil {
		f.int32Range("period_days", *in.PeriodDays, 1)
	}
	if in.AmountMinor < 1 {
		f.add("amount_minor", "out_of_range")
	}
	if !currencyCode.MatchString(in.Currency) {
		f.add("currency", "invalid_format")
	}
	if err := f.err(); err != nil {
		return Price{}, err
	}
	var out Price
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		p, err := q.LockPlan(ctx, plan)
		if err != nil {
			return notFound(err)
		}
		if p.Kind == "free" {
			return apierr.InvalidState
		}
		var f fields
		if (p.Kind == "one_time") != (in.Period == "one_time") {
			f.add("period", "not_allowed")
		}
		if in.PeriodDays != nil && in.Period != "one_time" {
			f.add("period_days", "not_allowed")
		}
		cur, err := siteCurrency(ctx, q)
		if err != nil {
			return err
		}
		if in.Currency != cur {
			f.add("currency", "not_allowed")
		}
		if err := f.err(); err != nil {
			return err
		}
		old, err := q.DiscontinuePeriodPrice(ctx, sqlc.DiscontinuePeriodPriceParams{PlanID: plan, Period: in.Period})
		if err != nil {
			return err
		}
		r, err := q.InsertPlanPrice(ctx, sqlc.InsertPlanPriceParams{PlanID: plan, Period: in.Period,
			PeriodDays: ptr32(in.PeriodDays), AmountMinor: in.AmountMinor, Currency: in.Currency})
		if err != nil {
			return err
		}
		out = Price(r)
		if err := q.BumpPlanVersion(ctx, plan); err != nil {
			return err
		}
		var discontinued *uuid.UUID
		if len(old) > 0 {
			discontinued = &old[0]
		}
		return audit.Record(ctx, q, audit.Entry{Action: "plan_price.create", TargetType: "plan_price", TargetID: r.ID.String(),
			Diff: audit.Values(map[string]any{"plan_id": plan, "period": r.Period, "period_days": r.PeriodDays,
				"amount_minor": r.AmountMinor, "currency": r.Currency, "discontinued_price_id": discontinued})})
	})
	return out, err
}

// DiscontinuePrice 停售价格行（BIL-01）。已停售返回 409 invalid_state；在售套餐的最后一个在售价格行
// 不能停售（409 invalid_state，应先下架套餐）。套餐的版本加 1。
func (s *Service) DiscontinuePrice(ctx context.Context, plan, id uuid.UUID, ifMatch string) (Price, error) {
	var out Price
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		p, err := q.LockPlan(ctx, plan)
		if err != nil {
			return notFound(err)
		}
		r, err := q.LockPlanPrice(ctx, sqlc.LockPlanPriceParams{ID: id, PlanID: plan})
		if err != nil {
			return notFound(err)
		}
		out = Price(r)
		if err := checkIfMatch(ifMatch, PriceETag(out)); err != nil {
			return err
		}
		if !out.OnSale {
			return apierr.InvalidState
		}
		if p.Status == "on_sale" {
			usage, err := q.PlanUsage(ctx, plan)
			if err != nil {
				return err
			}
			if usage.OnSalePrices <= 1 {
				return apierr.InvalidState
			}
		}
		if err := q.DiscontinuePlanPrice(ctx, id); err != nil {
			return err
		}
		out.OnSale = false
		if err := q.BumpPlanVersion(ctx, plan); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: "plan_price.discontinue", TargetType: "plan_price", TargetID: id.String(),
			Diff: audit.Changes(map[string]any{"is_on_sale": true}, map[string]any{"is_on_sale": false})})
	})
	return out, err
}
