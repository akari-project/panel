// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

type planOut struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	Tier                   int      `json:"tier"`
	Kind                   string   `json:"kind"`
	Status                 string   `json:"status"`
	Sort                   int      `json:"sort"`
	LocationGroupIDs       []string `json:"location_group_ids"`
	Prices                 []priceOut
	ActiveEntitlementCount int `json:"active_entitlement_count"`
}

type priceOut struct {
	ID          string `json:"id"`
	Period      string `json:"period"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	IsOnSale    bool   `json:"is_on_sale"`
}

type groupOut struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	MinTier   *int     `json:"min_tier"`
	HostCount int      `json:"host_count"`
	PlanIDs   []string `json:"plan_ids"`
}

// newCatalogEnv 是站点已初始化结算货币与时区的测试环境（站点初始化见 M1-09，CONV-08、CONV-26）。
func newCatalogEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO settings (key, value) VALUES ('site_currency', '"CNY"'), ('site_timezone', '"Asia/Shanghai"')`); err != nil {
		t.Fatal(err)
	}
	return e
}

// must 执行请求并要求状态码为 want，out 非空时解码响应体。
func (e *env) must(t *testing.T, r req, want int, out any) *httptest.ResponseRecorder {
	t.Helper()
	w := e.do(r)
	if w.Code != want {
		t.Fatalf("%s %s: %d, want %d: %s", r.method, r.path, w.Code, want, w.Body)
	}
	if out != nil {
		decode(t, w, out)
	}
	return w
}

// wantError 要求错误响应的状态码与 code；field 非空时同时比较第一项 errors。
func wantError(t *testing.T, w *httptest.ResponseRecorder, status int, code, field, fieldErr string) {
	t.Helper()
	if w.Code != status || problemCode(t, w) != code {
		t.Fatalf("got %d %s, want %d %s", w.Code, w.Body, status, code)
	}
	if field != "" {
		if f, c := fieldCode(t, w); f != field || c != fieldErr {
			t.Fatalf("errors[0] = %s %s, want %s %s: %s", f, c, field, fieldErr, w.Body)
		}
	}
}

func (e *env) createGroup(t *testing.T, a *admin, name string, minTier *int) (groupOut, string) {
	t.Helper()
	var g groupOut
	w := e.must(t, req{method: "POST", path: "/v1/location-groups", as: a,
		body: map[string]any{"name": name, "min_tier": minTier}}, 201, &g)
	return g, w.Header().Get("ETag")
}

func (e *env) createPlan(t *testing.T, a *admin, body map[string]any) (planOut, string) {
	t.Helper()
	base := map[string]any{"name": "Standard", "tier": 1, "kind": "recurring", "bytes_per_cycle": 100 << 30, "device_limit": 3}
	for k, v := range body {
		base[k] = v
	}
	var p planOut
	w := e.must(t, req{method: "POST", path: "/v1/plans", as: a, body: base}, 201, &p)
	return p, w.Header().Get("ETag")
}

func (e *env) createPrice(t *testing.T, a *admin, plan, period string, amount int64) priceOut {
	t.Helper()
	var p priceOut
	e.must(t, req{method: "POST", path: "/v1/plans/" + plan + "/prices", as: a,
		body: map[string]any{"period": period, "amount_minor": amount, "currency": "CNY"}}, 201, &p)
	return p
}

func (e *env) planETag(t *testing.T, a *admin, id string) string {
	t.Helper()
	return e.must(t, req{method: "GET", path: "/v1/plans/" + id, as: a}, 200, nil).Header().Get("ETag")
}

// holder 建立一个持有该套餐当前权益的账号。
func (e *env) holder(t *testing.T, plan string) uuid.UUID {
	t.Helper()
	a := e.account(t, false)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO entitlements (account_id, plan_id, status, starts_at, cycle_start,
		bytes_limit, device_limit, reset_policy) VALUES ($1, $2, 'active', $3, $3, 0, 1, 'never')`, a.id, plan, t0); err != nil {
		t.Fatal(err)
	}
	return a.id
}

// host 建立一个节点并加入线路组。
func (e *env) host(t *testing.T, group, region string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO nodes (name, region_code, public_host) VALUES ('n', $1, 'h.example.com')
		RETURNING id`, region).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO node_group_members (node_id, group_id) VALUES ($1, $2)`, id, group); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) events(t *testing.T, topic, key, id string) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM outbox WHERE topic = $1 AND payload->>$2 = $3 AND (payload->>'schema_version')::int = 1`,
		topic, key, id)
}

// 套餐的创建、读取、修改与强 ETag（CONV-13、CONV-28）；tier 变化写 plan.access_changed（ACS-05）。
func TestPlanLifecycle(t *testing.T) {
	e := newCatalogEnv(t)
	op := e.staff(t, "operator")
	g, _ := e.createGroup(t, op, "Asia", nil)
	p, etag := e.createPlan(t, op, map[string]any{"location_group_ids": []string{g.ID}, "description": "d"})
	if p.Status != "draft" || len(p.LocationGroupIDs) != 1 || len(p.Prices) != 0 || etag == "" {
		t.Fatalf("created %+v etag %q", p, etag)
	}
	if e.events(t, "plan.access_changed", "plan_id", p.ID) != 0 {
		t.Fatal("new plan must not emit plan.access_changed")
	}
	path := "/v1/plans/" + p.ID
	e.must(t, req{method: "GET", path: path, as: op, header: map[string]string{"If-None-Match": etag}}, 304, nil)

	wantError(t, e.do(req{method: "PATCH", path: path, as: op, body: map[string]any{"name": "x"}}), 428, "precondition_required", "", "")
	wantError(t, e.do(req{method: "PATCH", path: path, as: op, body: map[string]any{"name": "x"},
		header: map[string]string{"If-Match": `"999"`}}), 409, "conflict", "", "")
	wantError(t, e.do(req{method: "PATCH", path: path, as: op, body: map[string]any{},
		header: map[string]string{"If-Match": etag}}), 400, "invalid_request", "", "")

	// 没有变化：ETag 不变，不写审计。
	w := e.must(t, req{method: "PATCH", path: path, as: op, body: map[string]any{"name": "Standard"},
		header: map[string]string{"If-Match": etag}}, 200, nil)
	if w.Header().Get("ETag") != etag {
		t.Fatal("no-op update changed the ETag")
	}
	w = e.must(t, req{method: "PATCH", path: path, as: op, body: map[string]any{"name": "Plus", "sort": 5},
		header: map[string]string{"If-Match": etag}}, 200, &p)
	etag2 := w.Header().Get("ETag")
	if p.Name != "Plus" || p.Sort != 5 || etag2 == etag {
		t.Fatalf("updated %+v etag %q", p, etag2)
	}
	if e.events(t, "plan.access_changed", "plan_id", p.ID) != 0 {
		t.Fatal("name change emitted plan.access_changed")
	}
	e.must(t, req{method: "PATCH", path: path, as: op, body: map[string]any{"tier": 3},
		header: map[string]string{"If-Match": etag2}}, 200, &p)
	if p.Tier != 3 || e.events(t, "plan.access_changed", "plan_id", p.ID) != 1 {
		t.Fatalf("tier change: %+v", p)
	}
	var diff string
	if err := e.pool.QueryRow(context.Background(), `SELECT diff::text FROM audit_logs WHERE action = 'plan.update' ORDER BY id DESC LIMIT 1`).
		Scan(&diff); err != nil || !strings.Contains(diff, `"tier": {"to": 3, "from": 1}`) {
		t.Fatalf("audit diff %s %v", diff, err)
	}
	got := strings.Join(e.auditActions(t), ",")
	if !strings.Contains(got, "location_group.create,plan.create,plan.update,plan.update") {
		t.Fatalf("audit = %s", got)
	}
}

// 套餐字段与状态规则（spec/11 11.1、BIL-15、BIL-21）。
func TestPlanRules(t *testing.T) {
	e := newCatalogEnv(t)
	op := e.staff(t, "operator")
	for name, c := range map[string]struct {
		body              map[string]any
		status            int
		code, field, kind string
	}{
		"tier 0 recurring":  {map[string]any{"tier": 0}, 400, "invalid_request", "tier", "out_of_range"},
		"free with tier":    {map[string]any{"kind": "free", "tier": 1}, 400, "invalid_request", "tier", "out_of_range"},
		"empty name":        {map[string]any{"name": " "}, 400, "invalid_request", "name", "required"},
		"long name":         {map[string]any{"name": strings.Repeat("名", 101)}, 400, "invalid_request", "name", "too_long"},
		"device limit 0":    {map[string]any{"device_limit": 0}, 400, "invalid_request", "device_limit", "out_of_range"},
		"negative bytes":    {map[string]any{"bytes_per_cycle": -1}, 400, "invalid_request", "bytes_per_cycle", "out_of_range"},
		"speed 0":           {map[string]any{"speed_limit_mbps": 0}, 400, "invalid_request", "speed_limit_mbps", "out_of_range"},
		"bad kind":          {map[string]any{"kind": "weekly"}, 400, "invalid_request", "kind", "invalid_format"},
		"bad reset":         {map[string]any{"reset_policy": "daily"}, 400, "invalid_request", "reset_policy", "invalid_format"},
		"unknown group":     {map[string]any{"location_group_ids": []string{uuid.NewString()}}, 400, "invalid_request", "location_group_ids", "not_allowed"},
		"on sale at create": {map[string]any{"status": "on_sale"}, 409, "invalid_state", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			body := map[string]any{"name": "P", "tier": 1, "kind": "recurring", "bytes_per_cycle": 0, "device_limit": 1}
			for k, v := range c.body {
				body[k] = v
			}
			wantError(t, e.do(req{method: "POST", path: "/v1/plans", as: op, body: body}), c.status, c.code, c.field, c.kind)
		})
	}

	// 至多一个免费套餐；免费套餐在 M1-04 中不能上架。
	free, freeTag := e.createPlan(t, op, map[string]any{"name": "Free", "kind": "free", "tier": 0})
	wantError(t, e.do(req{method: "POST", path: "/v1/plans", as: op,
		body: map[string]any{"name": "Free 2", "kind": "free", "tier": 0, "bytes_per_cycle": 0, "device_limit": 1}}),
		400, "invalid_request", "kind", "taken")
	wantError(t, e.do(req{method: "POST", path: "/v1/plans/" + free.ID + "/prices", as: op,
		body: map[string]any{"period": "month", "amount_minor": 100, "currency": "CNY"}}), 409, "invalid_state", "", "")
	// 免费套餐的状态不受限制（BIL-15）；改为非免费类型后按修改后的状态检查在售价格行（BIL-26）。
	w := e.must(t, req{method: "PATCH", path: "/v1/plans/" + free.ID, as: op, body: map[string]any{"status": "on_sale"},
		header: map[string]string{"If-Match": freeTag}}, 200, &free)
	freeTag = w.Header().Get("ETag")
	if free.Status != "on_sale" {
		t.Fatalf("free status = %s", free.Status)
	}
	wantError(t, e.do(req{method: "PATCH", path: "/v1/plans/" + free.ID, as: op, body: map[string]any{"kind": "recurring", "tier": 1},
		header: map[string]string{"If-Match": freeTag}}), 409, "invalid_state", "", "")
	// 被 free_plan_id 引用的套餐不能修改类型、不能删除。
	w = e.must(t, req{method: "PATCH", path: "/v1/plans/" + free.ID, as: op, body: map[string]any{"status": "draft"},
		header: map[string]string{"If-Match": freeTag}}, 200, nil)
	freeTag = w.Header().Get("ETag")
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO settings (key, value) VALUES ('free_plan_id', to_jsonb($1::text))`, free.ID); err != nil {
		t.Fatal(err)
	}
	wantError(t, e.do(req{method: "PATCH", path: "/v1/plans/" + free.ID, as: op, body: map[string]any{"kind": "recurring", "tier": 1},
		header: map[string]string{"If-Match": freeTag}}), 409, "invalid_state", "", "")
	wantError(t, e.do(req{method: "DELETE", path: "/v1/plans/" + free.ID, as: op,
		header: map[string]string{"If-Match": freeTag}}), 409, "invalid_state", "", "")
	if _, err := e.pool.Exec(context.Background(), `UPDATE settings SET value = 'null' WHERE key = 'free_plan_id'`); err != nil {
		t.Fatal(err)
	}
	e.must(t, req{method: "DELETE", path: "/v1/plans/" + free.ID, as: op, header: map[string]string{"If-Match": freeTag}}, 204, nil)

	// 上架需要在售价格行；有价格行后不能修改类型。
	p, etag := e.createPlan(t, op, nil)
	path := "/v1/plans/" + p.ID
	wantError(t, e.do(req{method: "PATCH", path: path, as: op, body: map[string]any{"status": "on_sale"},
		header: map[string]string{"If-Match": etag}}), 409, "invalid_state", "", "")
	e.must(t, req{method: "PATCH", path: path, as: op, body: map[string]any{"kind": "one_time"},
		header: map[string]string{"If-Match": etag}}, 200, &p)
	if p.Kind != "one_time" {
		t.Fatalf("kind = %s", p.Kind)
	}
	e.createPrice(t, op, p.ID, "one_time", 1000)
	etag = e.planETag(t, op, p.ID)
	wantError(t, e.do(req{method: "PATCH", path: path, as: op, body: map[string]any{"kind": "recurring"},
		header: map[string]string{"If-Match": etag}}), 409, "invalid_state", "", "")
	wantError(t, e.do(req{method: "PATCH", path: path, as: op, body: map[string]any{"kind": "free", "tier": 0},
		header: map[string]string{"If-Match": etag}}), 409, "invalid_state", "", "")
	w = e.must(t, req{method: "PATCH", path: path, as: op, body: map[string]any{"status": "on_sale"},
		header: map[string]string{"If-Match": etag}}, 200, &p)
	if p.Status != "on_sale" {
		t.Fatalf("status = %s", p.Status)
	}
	// 有权益的套餐不能改回 draft，可以下架。
	e.holder(t, p.ID)
	etag = w.Header().Get("ETag")
	wantError(t, e.do(req{method: "PATCH", path: path, as: op, body: map[string]any{"status": "draft"},
		header: map[string]string{"If-Match": etag}}), 409, "invalid_state", "", "")
	e.must(t, req{method: "PATCH", path: path, as: op, body: map[string]any{"status": "archived"},
		header: map[string]string{"If-Match": etag}}, 200, &p)
	if p.ActiveEntitlementCount != 1 {
		t.Fatalf("active_entitlement_count = %d", p.ActiveEntitlementCount)
	}
}

// 删除：只能删除没有价格行与权益的套餐（契约 deletePlan）；关联的线路组随之解除。
func TestDeletePlan(t *testing.T) {
	e := newCatalogEnv(t)
	op := e.staff(t, "operator")
	g, gTag := e.createGroup(t, op, "Asia", nil)
	p, etag := e.createPlan(t, op, map[string]any{"location_group_ids": []string{g.ID}})
	if g2 := e.must(t, req{method: "GET", path: "/v1/location-groups/" + g.ID, as: op}, 200, nil).Header().Get("ETag"); g2 != gTag {
		t.Fatal("linking a plan changed the group ETag (plan_ids is derived, CON-05)")
	}
	wantError(t, e.do(req{method: "DELETE", path: "/v1/plans/" + p.ID, as: op}), 428, "precondition_required", "", "")
	e.must(t, req{method: "DELETE", path: "/v1/plans/" + p.ID, as: op, header: map[string]string{"If-Match": etag}}, 204, nil)
	e.must(t, req{method: "GET", path: "/v1/plans/" + p.ID, as: op}, 404, nil)
	var got groupOut
	e.must(t, req{method: "GET", path: "/v1/location-groups/" + g.ID, as: op}, 200, &got)
	if len(got.PlanIDs) != 0 {
		t.Fatalf("plan_ids = %v", got.PlanIDs)
	}

	priced, _ := e.createPlan(t, op, nil)
	e.createPrice(t, op, priced.ID, "month", 100)
	wantError(t, e.do(req{method: "DELETE", path: "/v1/plans/" + priced.ID, as: op,
		header: map[string]string{"If-Match": e.planETag(t, op, priced.ID)}}), 409, "invalid_state", "", "")
	held, _ := e.createPlan(t, op, nil)
	e.holder(t, held.ID)
	wantError(t, e.do(req{method: "DELETE", path: "/v1/plans/" + held.ID, as: op,
		header: map[string]string{"If-Match": e.planETag(t, op, held.ID)}}), 409, "invalid_state", "", "")
	if n := e.count(t, `SELECT count(*) FROM audit_logs WHERE action = 'plan.delete' AND target_id = $1`, p.ID); n != 1 {
		t.Fatal("plan.delete not audited")
	}
}

// 价格行只新建与停售（BIL-01，M1-04 验收 1）：改价即新建，旧行在同一事务中停售；周期与类型一致；币种为站点货币。
func TestPlanPrices(t *testing.T) {
	e := newCatalogEnv(t)
	op := e.staff(t, "operator")
	p, etag := e.createPlan(t, op, nil)
	prices := "/v1/plans/" + p.ID + "/prices"
	for name, c := range map[string]struct {
		body        map[string]any
		field, code string
	}{
		"zero amount":         {map[string]any{"period": "month", "amount_minor": 0, "currency": "CNY"}, "amount_minor", "out_of_range"},
		"bad currency":        {map[string]any{"period": "month", "amount_minor": 1, "currency": "cny"}, "currency", "invalid_format"},
		"other currency":      {map[string]any{"period": "month", "amount_minor": 1, "currency": "USD"}, "currency", "not_allowed"},
		"period for one_time": {map[string]any{"period": "one_time", "amount_minor": 1, "currency": "CNY"}, "period", "not_allowed"},
		"period_days":         {map[string]any{"period": "month", "period_days": 30, "amount_minor": 1, "currency": "CNY"}, "period_days", "not_allowed"},
		"bad period":          {map[string]any{"period": "week", "amount_minor": 1, "currency": "CNY"}, "period", "invalid_format"},
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, e.do(req{method: "POST", path: prices, as: op, body: c.body}), 400, "invalid_request", c.field, c.code)
		})
	}
	wantError(t, e.do(req{method: "POST", path: "/v1/plans/" + uuid.NewString() + "/prices", as: op,
		body: map[string]any{"period": "month", "amount_minor": 1, "currency": "CNY"}}), 404, "not_found", "", "")

	first := e.createPrice(t, op, p.ID, "month", 3000)
	year := e.createPrice(t, op, p.ID, "year", 30000)
	etag2 := e.planETag(t, op, p.ID)
	if etag2 == etag {
		t.Fatal("new price did not change the plan ETag")
	}
	firstTag := e.must(t, req{method: "GET", path: prices + "/" + first.ID, as: op}, 200, nil).Header().Get("ETag")
	second := e.createPrice(t, op, p.ID, "month", 3500)
	var got priceOut
	w := e.must(t, req{method: "GET", path: prices + "/" + first.ID, as: op}, 200, &got)
	if got.IsOnSale || w.Header().Get("ETag") == firstTag {
		t.Fatalf("old row after re-pricing: %+v %s", got, w.Header().Get("ETag"))
	}
	e.must(t, req{method: "GET", path: prices + "/" + first.ID, as: op, header: map[string]string{"If-None-Match": w.Header().Get("ETag")}}, 304, nil)
	var diff string
	if err := e.pool.QueryRow(context.Background(), `SELECT diff->>'discontinued_price_id' FROM audit_logs
		WHERE action = 'plan_price.create' AND target_id = $1`, second.ID).Scan(&diff); err != nil || diff != first.ID {
		t.Fatalf("discontinued_price_id = %q %v", diff, err)
	}
	var plan planOut
	e.must(t, req{method: "GET", path: "/v1/plans/" + p.ID, as: op}, 200, &plan)
	if len(plan.Prices) != 2 {
		t.Fatalf("on-sale prices = %+v", plan.Prices)
	}
	var list struct {
		Items      []priceOut `json:"items"`
		NextCursor *string    `json:"next_cursor"`
	}
	e.must(t, req{method: "GET", path: prices + "?limit=2", as: op}, 200, &list)
	if len(list.Items) != 2 || list.Items[0].ID != second.ID || list.Items[1].ID != year.ID || list.NextCursor == nil {
		t.Fatalf("page 1 = %+v", list)
	}
	e.must(t, req{method: "GET", path: prices + "?limit=2&cursor=" + *list.NextCursor, as: op}, 200, &list)
	if len(list.Items) != 1 || list.Items[0].ID != first.ID || list.NextCursor != nil {
		t.Fatalf("page 2 = %+v", list)
	}
	e.must(t, req{method: "GET", path: prices + "?is_on_sale=false", as: op}, 200, &list)
	if len(list.Items) != 1 || list.Items[0].ID != first.ID {
		t.Fatalf("discontinued = %+v", list.Items)
	}

	// 停售：只接受 false；已停售 409；在售套餐的最后一个在售价格行不能停售。
	yearPath := prices + "/" + year.ID
	yearTag := e.must(t, req{method: "GET", path: yearPath, as: op}, 200, nil).Header().Get("ETag")
	wantError(t, e.do(req{method: "PATCH", path: yearPath, as: op, body: map[string]any{"is_on_sale": true},
		header: map[string]string{"If-Match": yearTag}}), 400, "invalid_request", "is_on_sale", "not_allowed")
	wantError(t, e.do(req{method: "PATCH", path: yearPath, as: op, body: map[string]any{"is_on_sale": false}}),
		428, "precondition_required", "", "")
	wantError(t, e.do(req{method: "PATCH", path: yearPath, as: op, body: map[string]any{"is_on_sale": false},
		header: map[string]string{"If-Match": `"stale"`}}), 409, "conflict", "", "")
	w = e.must(t, req{method: "PATCH", path: yearPath, as: op, body: map[string]any{"is_on_sale": false},
		header: map[string]string{"If-Match": yearTag}}, 200, &got)
	if got.IsOnSale || w.Header().Get("ETag") == yearTag {
		t.Fatalf("after discontinue: %+v %s", got, w.Header().Get("ETag"))
	}
	wantError(t, e.do(req{method: "PATCH", path: yearPath, as: op, body: map[string]any{"is_on_sale": false},
		header: map[string]string{"If-Match": w.Header().Get("ETag")}}), 409, "invalid_state", "", "")
	e.must(t, req{method: "PATCH", path: "/v1/plans/" + p.ID, as: op, body: map[string]any{"status": "on_sale"},
		header: map[string]string{"If-Match": e.planETag(t, op, p.ID)}}, 200, nil)
	secondTag := e.must(t, req{method: "GET", path: prices + "/" + second.ID, as: op}, 200, nil).Header().Get("ETag")
	wantError(t, e.do(req{method: "PATCH", path: prices + "/" + second.ID, as: op, body: map[string]any{"is_on_sale": false},
		header: map[string]string{"If-Match": secondTag}}), 409, "invalid_state", "", "")
	if n := e.count(t, `SELECT count(*) FROM audit_logs WHERE action = 'plan_price.discontinue' AND target_type = 'plan_price'`); n != 1 {
		t.Fatalf("plan_price.discontinue audits = %d", n)
	}
}

// 并发新建同一周期的价格行：按套餐行串行化，最后只有一行在售（BIL-01）。
func TestConcurrentPriceCreate(t *testing.T) {
	e := newCatalogEnv(t)
	op := e.staff(t, "operator")
	p, _ := e.createPlan(t, op, nil)
	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Go(func() {
			w := e.do(req{method: "POST", path: "/v1/plans/" + p.ID + "/prices", as: op,
				body: map[string]any{"period": "month", "amount_minor": 1000 + i, "currency": "CNY"}})
			codes[i] = w.Code
		})
	}
	wg.Wait()
	for i, c := range codes {
		if c != 201 {
			t.Errorf("request %d: %d", i, c)
		}
	}
	if on := e.count(t, `SELECT count(*) FROM plan_prices WHERE plan_id = $1 AND on_sale`, p.ID); on != 1 {
		t.Fatalf("on-sale rows = %d", on)
	}
	if all := e.count(t, `SELECT count(*) FROM plan_prices WHERE plan_id = $1`, p.ID); all != n {
		t.Fatalf("rows = %d", all)
	}
}

// 线路组关联（BIL-04）：添加幂等；移除为敏感操作（原因 400、step-up 401）；两者写 plan.access_changed。
func TestPlanLocationGroupLinks(t *testing.T) {
	e := newCatalogEnv(t)
	op := e.staff(t, "operator")
	g, _ := e.createGroup(t, op, "Asia", nil)
	p, etag := e.createPlan(t, op, nil)
	link := "/v1/plans/" + p.ID + "/location-groups/" + g.ID
	wantError(t, e.do(req{method: "PUT", path: link, as: op}), 428, "precondition_required", "", "")
	wantError(t, e.do(req{method: "PUT", path: "/v1/plans/" + p.ID + "/location-groups/" + uuid.NewString(), as: op,
		header: map[string]string{"If-Match": etag}}), 404, "not_found", "", "")
	w := e.must(t, req{method: "PUT", path: link, as: op, header: map[string]string{"If-Match": etag}}, 200, &p)
	etag = w.Header().Get("ETag")
	if len(p.LocationGroupIDs) != 1 || e.events(t, "plan.access_changed", "plan_id", p.ID) != 1 {
		t.Fatalf("after add: %+v", p)
	}
	w = e.must(t, req{method: "PUT", path: link, as: op, header: map[string]string{"If-Match": etag}}, 200, nil)
	if w.Header().Get("ETag") != etag || e.events(t, "plan.access_changed", "plan_id", p.ID) != 1 {
		t.Fatal("re-adding changed the plan")
	}
	// 仍被套餐引用的线路组不能删除（ACS-06）。
	gTag := e.must(t, req{method: "GET", path: "/v1/location-groups/" + g.ID, as: op}, 200, nil).Header().Get("ETag")
	wantError(t, e.do(req{method: "DELETE", path: "/v1/location-groups/" + g.ID, as: op,
		header: map[string]string{"If-Match": gTag}}), 409, "invalid_state", "", "")

	h := map[string]string{"If-Match": etag}
	wantError(t, e.do(req{method: "DELETE", path: link, as: op, header: h}), 400, "invalid_request", "Audit-Reason", "required")
	h["Audit-Reason"] = "retire"
	wantError(t, e.do(req{method: "DELETE", path: link, as: op, header: h}), 401, "mfa_required", "", "")
	if e.count(t, `SELECT count(*) FROM plan_groups`) != 1 {
		t.Fatal("unlinked before step-up")
	}
	e.stepUp(t, op)
	e.must(t, req{method: "DELETE", path: link, as: op, sensitive: true, header: h}, 200, &p)
	if len(p.LocationGroupIDs) != 0 || e.events(t, "plan.access_changed", "plan_id", p.ID) != 2 {
		t.Fatalf("after remove: %+v", p)
	}
	h["If-Match"] = e.planETag(t, op, p.ID)
	wantError(t, e.do(req{method: "DELETE", path: link, as: op, sensitive: true, header: h}), 404, "not_found", "", "")
	var reason string
	if err := e.pool.QueryRow(context.Background(), `SELECT r.body FROM audit_logs l JOIN reason_texts r ON r.id = l.reason_id
		WHERE l.action = 'plan_location_group.delete' AND l.target_type = 'plan' AND l.target_id = $1`, p.ID).Scan(&reason); err != nil || reason != "retire" {
		t.Fatalf("reason = %q %v", reason, err)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_logs WHERE action = 'plan_location_group.create' AND diff->>'location_group_id' = $1`, g.ID); n != 1 {
		t.Fatalf("plan_location_group.create audits = %d", n)
	}
}

// 线路组（ACS-05、ACS-06）：名称唯一；min_tier 变化写 location_group.changed；分页；有节点时不能删除。
func TestLocationGroups(t *testing.T) {
	e := newCatalogEnv(t)
	op := e.staff(t, "operator")
	two := 2
	g, etag := e.createGroup(t, op, "Asia", &two)
	if g.MinTier == nil || *g.MinTier != 2 || g.PlanIDs == nil {
		t.Fatalf("created %+v", g)
	}
	wantError(t, e.do(req{method: "POST", path: "/v1/location-groups", as: op, body: map[string]any{"name": "Asia"}}),
		400, "invalid_request", "name", "taken")
	wantError(t, e.do(req{method: "POST", path: "/v1/location-groups", as: op, body: map[string]any{"name": "B", "min_tier": -1}}),
		400, "invalid_request", "min_tier", "out_of_range")
	path := "/v1/location-groups/" + g.ID
	e.must(t, req{method: "GET", path: path, as: op, header: map[string]string{"If-None-Match": etag}}, 304, nil)
	w := e.must(t, req{method: "PATCH", path: path, as: op, body: map[string]any{"description": "HK, JP"},
		header: map[string]string{"If-Match": etag}}, 200, nil)
	if e.events(t, "location_group.changed", "location_group_id", g.ID) != 0 {
		t.Fatal("description change emitted location_group.changed")
	}
	w = e.must(t, req{method: "PATCH", path: path, as: op, body: map[string]any{"min_tier": nil},
		header: map[string]string{"If-Match": w.Header().Get("ETag")}}, 200, &g)
	if g.MinTier != nil || e.events(t, "location_group.changed", "location_group_id", g.ID) != 1 {
		t.Fatalf("min_tier cleared: %+v", g)
	}
	other, _ := e.createGroup(t, op, "Europe", nil)
	e.createGroup(t, op, "America", nil)
	var list struct {
		Items      []groupOut `json:"items"`
		NextCursor *string    `json:"next_cursor"`
	}
	e.must(t, req{method: "GET", path: "/v1/location-groups?limit=2", as: op}, 200, &list)
	if len(list.Items) != 2 || list.Items[0].ID != g.ID || list.NextCursor == nil {
		t.Fatalf("page 1 = %+v", list)
	}
	e.must(t, req{method: "GET", path: "/v1/location-groups?limit=2&cursor=" + *list.NextCursor, as: op}, 200, &list)
	if len(list.Items) != 1 || list.NextCursor != nil {
		t.Fatalf("page 2 = %+v", list)
	}
	wantError(t, e.do(req{method: "GET", path: "/v1/location-groups?cursor=not-json", as: op}), 400, "invalid_request", "cursor", "invalid_format")

	e.host(t, other.ID, "JP")
	oTag := e.must(t, req{method: "GET", path: "/v1/location-groups/" + other.ID, as: op}, 200, &other).Header().Get("ETag")
	if other.HostCount != 1 {
		t.Fatalf("host_count = %d", other.HostCount)
	}
	wantError(t, e.do(req{method: "DELETE", path: "/v1/location-groups/" + other.ID, as: op,
		header: map[string]string{"If-Match": oTag}}), 409, "invalid_state", "", "")
	e.must(t, req{method: "DELETE", path: path, as: op, header: map[string]string{"If-Match": w.Header().Get("ETag")}}, 204, nil)
	e.must(t, req{method: "GET", path: path, as: op}, 404, nil)
	got := strings.Join(e.auditActions(t), ",")
	if !strings.Contains(got, "location_group.update,location_group.update") || !strings.HasSuffix(got, "location_group.delete") {
		t.Fatalf("audit = %s", got)
	}
}

// 套餐列表按 (sort, id) 分页，可按状态与类型筛选（CONV-11）。
func TestListPlans(t *testing.T) {
	e := newCatalogEnv(t)
	op := e.staff(t, "operator")
	var ids []string
	for i, s := range []int{20, 10, 10} {
		p, _ := e.createPlan(t, op, map[string]any{"name": fmt.Sprintf("P%d", i), "sort": s})
		ids = append(ids, p.ID)
	}
	e.createPlan(t, op, map[string]any{"name": "Free", "kind": "free", "tier": 0, "sort": 30})
	var list struct {
		Items      []planOut `json:"items"`
		NextCursor *string   `json:"next_cursor"`
	}
	e.must(t, req{method: "GET", path: "/v1/plans?limit=2&kind=recurring", as: op}, 200, &list)
	if len(list.Items) != 2 || list.Items[0].ID != ids[1] || list.Items[1].ID != ids[2] || list.NextCursor == nil {
		t.Fatalf("page 1 = %+v", list)
	}
	e.must(t, req{method: "GET", path: "/v1/plans?limit=2&kind=recurring&cursor=" + *list.NextCursor, as: op}, 200, &list)
	if len(list.Items) != 1 || list.Items[0].ID != ids[0] || list.NextCursor != nil {
		t.Fatalf("page 2 = %+v", list)
	}
	e.must(t, req{method: "GET", path: "/v1/plans?status=draft", as: op}, 200, &list)
	if len(list.Items) != 4 {
		t.Fatalf("draft = %d", len(list.Items))
	}
	wantError(t, e.do(req{method: "GET", path: "/v1/plans?limit=201", as: op}), 400, "invalid_request", "limit", "out_of_range")
	support := e.staff(t, "support")
	wantError(t, e.do(req{method: "GET", path: "/v1/plans", as: support}), 403, "forbidden", "", "")
	wantError(t, e.do(req{method: "GET", path: "/v1/location-groups", as: support}), 403, "forbidden", "", "")
}

// 影响预览（CON-07，M1-04 验收 2）：只计算，不产生副作用。
func TestImpactPreview(t *testing.T) {
	e := newCatalogEnv(t)
	op := e.staff(t, "operator")
	three := 3
	asia, _ := e.createGroup(t, op, "Asia", nil)
	premium, _ := e.createGroup(t, op, "Premium", &three)
	e.host(t, asia.ID, "HK")
	e.host(t, asia.ID, "JP")
	e.host(t, premium.ID, "US")
	p, _ := e.createPlan(t, op, map[string]any{"tier": 2, "location_group_ids": []string{asia.ID, premium.ID}})
	e.holder(t, p.ID)
	e.holder(t, p.ID)
	other, _ := e.createPlan(t, op, map[string]any{"name": "Other", "location_group_ids": []string{asia.ID}})
	e.holder(t, other.ID)

	type impact struct {
		Accounts   int    `json:"affected_account_count"`
		Hosts      int    `json:"affected_host_count"`
		ComputedAt string `json:"computed_at"`
	}
	preview := func(path string, body map[string]any) impact {
		t.Helper()
		var out impact
		e.must(t, req{method: "POST", path: path, as: op, body: body}, 200, &out)
		return out
	}
	planPath := "/v1/plans/" + p.ID + "/impact"
	before := e.count(t, `SELECT count(*) FROM outbox`) + e.count(t, `SELECT count(*) FROM audit_logs`)
	for name, c := range map[string]struct {
		body            map[string]any
		accounts, hosts int
	}{
		"remove asia":    {map[string]any{"location_group_ids": []string{premium.ID}}, 2, 2},
		"same groups":    {map[string]any{"location_group_ids": []string{premium.ID, asia.ID}}, 2, 0},
		"tier unlocks":   {map[string]any{"tier": 3}, 2, 1},
		"tier no effect": {map[string]any{"tier": 1}, 2, 0},
		"status":         {map[string]any{"status": "hidden"}, 2, 0},
		"rollout":        {map[string]any{"rollout_fields": []string{"device_limit"}}, 2, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := preview(planPath, c.body); got.Accounts != c.accounts || got.Hosts != c.hosts || got.ComputedAt == "" {
				t.Fatalf("got %+v, want %d accounts %d hosts", got, c.accounts, c.hosts)
			}
		})
	}
	wantError(t, e.do(req{method: "POST", path: planPath, as: op, body: map[string]any{}}), 400, "invalid_request", "", "")
	wantError(t, e.do(req{method: "POST", path: planPath, as: op, body: map[string]any{"location_group_ids": []string{uuid.NewString()}}}),
		400, "invalid_request", "location_group_ids", "not_allowed")
	wantError(t, e.do(req{method: "POST", path: "/v1/plans/" + uuid.NewString() + "/impact", as: op, body: map[string]any{"tier": 1}}),
		404, "not_found", "", "")

	groupPath := "/v1/location-groups/" + premium.ID + "/impact"
	if got := preview(groupPath, map[string]any{"min_tier": 2}); got.Accounts != 2 || got.Hosts != 1 {
		t.Fatalf("lower min_tier: %+v", got)
	}
	if got := preview(groupPath, map[string]any{"min_tier": 4}); got.Accounts != 0 || got.Hosts != 0 {
		t.Fatalf("raise min_tier, nobody had access: %+v", got)
	}
	if got := preview("/v1/location-groups/"+asia.ID+"/impact", map[string]any{"min_tier": 2}); got.Accounts != 1 || got.Hosts != 2 {
		t.Fatalf("min_tier cuts the tier-1 plan: %+v", got)
	}
	if got := preview("/v1/location-groups/"+asia.ID+"/impact", map[string]any{"is_deletion": true}); got.Accounts != 0 || got.Hosts != 0 {
		t.Fatalf("deletion: %+v", got)
	}
	wantError(t, e.do(req{method: "POST", path: groupPath, as: op, body: map[string]any{"add_host_ids": []string{uuid.NewString()}}}),
		400, "invalid_request", "add_host_ids", "not_allowed")
	if after := e.count(t, `SELECT count(*) FROM outbox`) + e.count(t, `SELECT count(*) FROM audit_logs`); after != before {
		t.Fatal("impact preview had side effects")
	}
}

// 站点尚未初始化结算货币时不能定价（CONV-08）：409 invalid_state，不写入价格行。
func TestPriceNeedsSiteCurrency(t *testing.T) {
	e := newEnv(t)
	op := e.staff(t, "operator")
	p, _ := e.createPlan(t, op, nil)
	body := map[string]any{"period": "month", "amount_minor": 100, "currency": "CNY"}
	wantError(t, e.do(req{method: "POST", path: "/v1/plans/" + p.ID + "/prices", as: op, body: body}), 409, "invalid_state", "", "")
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO settings (key, value) VALUES ('site_currency', '"cny"')`); err != nil {
		t.Fatal(err)
	}
	wantError(t, e.do(req{method: "POST", path: "/v1/plans/" + p.ID + "/prices", as: op, body: body}), 409, "invalid_state", "", "")
	if n := e.count(t, `SELECT count(*) FROM plan_prices`); n != 0 {
		t.Fatalf("price rows = %d", n)
	}
}
