// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// 在售套餐（spec/11 BIL-15、BIL-21）：只列出有在售价格行的 on_sale 非免费套餐与在售价格行；location_count 为满足 min_tier 的
// 线路组中节点的地区数；不需要登录。
func TestListPlans(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := func(sql string, args ...any) uuid.UUID {
		t.Helper()
		var v uuid.UUID
		if err := e.pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// 先建价格行（draft 时），再上架。
	plan := func(name string, tier, sort int, status string) uuid.UUID {
		p := id(`INSERT INTO plans (name, tier, bytes_per_cycle, device_limit, sort) VALUES ($1, $2, 0, 1, $3) RETURNING id`, name, tier, sort)
		exec(`INSERT INTO plan_prices (plan_id, period, amount_minor, currency) VALUES ($1, 'month', 1000, 'CNY')`, p)
		exec(`UPDATE plans SET status = $2 WHERE id = $1`, p, status)
		return p
	}
	basic := plan("Basic", 1, 20, "on_sale")
	pro := plan("Pro", 3, 10, "on_sale")
	plan("Hidden", 1, 0, "hidden")
	plan("Draft", 1, 0, "draft")
	// 免费套餐无论状态都不列出；没有在售价格行的套餐不列出。
	exec(`INSERT INTO plans (name, tier, kind, status, bytes_per_cycle, device_limit) VALUES ('Free', 0, 'free', 'on_sale', 0, 1)`)
	unpriced := plan("Unpriced", 1, 0, "on_sale")
	exec(`UPDATE plan_prices SET on_sale = false WHERE plan_id = $1`, unpriced)
	exec(`UPDATE plan_prices SET on_sale = false WHERE plan_id = $1`, pro)
	exec(`INSERT INTO plan_prices (plan_id, period, amount_minor, currency) VALUES ($1, 'month', 2000, 'CNY'), ($1, 'year', 20000, 'CNY')`, pro)

	asia := id(`INSERT INTO location_groups (name) VALUES ('Asia') RETURNING id`)
	premium := id(`INSERT INTO location_groups (name, min_tier) VALUES ('Premium', 2) RETURNING id`)
	for _, r := range []struct {
		group  uuid.UUID
		region string
	}{{asia, "HK"}, {asia, "HK"}, {asia, "JP"}, {premium, "US"}} {
		n := id(`INSERT INTO nodes (name, region_code, public_host) VALUES ('n', $1, 'h') RETURNING id`, r.region)
		exec(`INSERT INTO node_group_members (node_id, group_id) VALUES ($1, $2)`, n, r.group)
	}
	for _, p := range []uuid.UUID{basic, pro} {
		exec(`INSERT INTO plan_groups (plan_id, group_id) VALUES ($1, $2), ($1, $3)`, p, asia, premium)
	}

	w := e.get("/v1/plans", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var out struct {
		Items []struct {
			ID            uuid.UUID `json:"id"`
			Name          string    `json:"name"`
			LocationCount int       `json:"location_count"`
			Prices        []struct {
				Period      string `json:"period"`
				AmountMinor int64  `json:"amount_minor"`
				Currency    string `json:"currency"`
			} `json:"prices"`
		} `json:"items"`
		AddonPrices []any `json:"addon_prices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 2 || out.Items[0].ID != pro || out.Items[1].ID != basic || out.AddonPrices == nil {
		t.Fatalf("items = %s", w.Body)
	}
	if p := out.Items[0]; p.LocationCount != 3 || len(p.Prices) != 2 || p.Prices[0].AmountMinor != 2000 || p.Prices[0].Currency != "CNY" {
		t.Fatalf("pro = %+v", p)
	}
	if b := out.Items[1]; b.LocationCount != 2 || len(b.Prices) != 1 {
		t.Fatalf("basic = %+v (tier 1 does not reach the tier-2 group)", b)
	}
}
