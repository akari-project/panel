// SPDX-License-Identifier: AGPL-3.0-or-later

// Package catalog 管理套餐、价格行与线路组（spec/11 11.1、BIL-01、BIL-04、BIL-15、BIL-21、ACS-05、ACS-06）。
//
//   - 每个写操作在一个事务中完成：锁定所属套餐或线路组的行，比较 If-Match（CONV-28），修改数据，
//     写审计日志（spec/31 CON-09）与 outbox 事件（CONV-22、CONV-34）。
//   - 强 ETag 由版本列生成（CONV-13）：套餐与线路组的 version 由本包加 1；价格行、线路组关联的变更同样使
//     所属套餐的版本加 1，线路组关联的变更同时使该线路组的版本加 1。派生的计数（当前权益数、节点数）不参与 ETag。
//   - 取锁顺序：套餐，然后线路组（按 ID 升序）。
//   - 只在访问关系确实变化时写事件：套餐 tier 变化或线路组关联增删写 plan.access_changed，
//     线路组 min_tier 变化写 location_group.changed（ACS-05、BIL-04）。
//
// 错误为 *apierr.Error：参数错误 400（errors[].field 为接口字段名），状态不允许 409 invalid_state，
// ETag 不一致 409 conflict，不存在 404。
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// 取值集合（spec/03 plans、plan_prices）。
var (
	Kinds         = []string{"recurring", "one_time", "free"}
	Statuses      = []string{"draft", "on_sale", "hidden", "archived"}
	ResetPolicies = []string{"purchase_anchor", "calendar_month", "never"}
	Periods       = []string{"month", "quarter", "half_year", "year", "one_time"}
	RolloutFields = []string{"bytes_per_cycle", "device_limit", "speed_limit_mbps", "reset_policy"}
)

// MaxName 是套餐与线路组名称的上限（Unicode 码点，契约 Plan.name、LocationGroup.name）。
const MaxName = 100

// MaxPublicPlans 是客户端接口在售套餐列表的上限（spec/30 listPlans，CONV-11）。
const MaxPublicPlans = 200

// DefaultCurrency 是 settings 中尚未设定 site_currency 时的结算货币（CONV-08）。
const DefaultCurrency = "CNY"

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

// Service 提供套餐、价格行与线路组的读写。
type Service struct {
	Pool  *pgxpool.Pool
	Clock clock.Clock
}

// Opt 是可选字段：Set 为假表示请求中没有该字段。
type Opt[T any] struct {
	Set bool
	V   T
}

// Some 返回已设置的可选字段。
func Some[T any](v T) Opt[T] { return Opt[T]{Set: true, V: v} }

func (o Opt[T]) or(d T) T {
	if o.Set {
		return o.V
	}
	return d
}

// ETag 是套餐或线路组的强 ETag（CONV-13），由版本列生成。
func ETag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

// PriceETag 是价格行的强 ETag。价格行只有 on_sale 可以改变，且只能由真变假一次（BIL-01），
// 因此在售为版本 1，停售为版本 2。
func PriceETag(p Price) string {
	if p.OnSale {
		return ETag(1)
	}
	return ETag(2)
}

// fields 收集参数错误，最后一并返回。
type fields []apierr.FieldError

func (f *fields) add(field, code string) { *f = append(*f, apierr.Field(field, code)) }

func (f fields) err() error {
	if len(f) == 0 {
		return nil
	}
	return apierr.Invalid(f...)
}

func (f *fields) name(field, v string) {
	switch n := utf8.RuneCountInString(v); {
	case strings.TrimSpace(v) == "":
		f.add(field, "required")
	case n > MaxName:
		f.add(field, "too_long")
	}
}

func (f *fields) enum(field, v string, allowed []string) {
	if !slices.Contains(allowed, v) {
		f.add(field, "invalid_format")
	}
}

// int32Range 检查 lo <= v <= math.MaxInt32。
func (f *fields) int32Range(field string, v int, lo int) {
	if v < lo || v > math.MaxInt32 {
		f.add(field, "out_of_range")
	}
}

// uniqueIDs 检查 ID 不重复（契约 uniqueItems）。
func (f *fields) uniqueIDs(field string, ids []uuid.UUID) {
	seen := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			f.add(field, "invalid_format")
			return
		}
		seen[id] = true
	}
}

func ptr32(p *int) *int32 {
	if p == nil {
		return nil
	}
	v := int32(*p)
	return &v
}

func checkIfMatch(ifMatch, etag string) error {
	if ifMatch != etag {
		return apierr.Conflict
	}
	return nil
}

func pgCode(err error) (code, constraint string) {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code, pg.ConstraintName
	}
	return "", ""
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apierr.NotFound
	}
	return err
}

// emit 写入 outbox 事件（CONV-22），载荷见 CONV-34。
func emit(ctx context.Context, q *sqlc.Queries, topic string, payload map[string]any) error {
	payload["schema_version"] = 1
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = q.InsertOutboxEvent(ctx, sqlc.InsertOutboxEventParams{Topic: topic, Payload: b, SchemaVersion: 1})
	return err
}

// siteCurrency 返回站点结算货币 site_currency（CONV-08）；尚未设定时为 DefaultCurrency。
func siteCurrency(ctx context.Context, q *sqlc.Queries) (string, error) {
	raw, err := q.GetSetting(ctx, "site_currency")
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultCurrency, nil
	}
	if err != nil {
		return "", err
	}
	var c string
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", err
	}
	return c, nil
}

// lockGroups 按 ID 升序使线路组的版本加 1（同时锁定这些行），用于线路组关联的变更。
func lockGroups(ctx context.Context, q *sqlc.Queries, ids []uuid.UUID) error {
	sorted := slices.Clone(ids)
	slices.SortFunc(sorted, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	for _, id := range sorted {
		if err := q.BumpLocationGroupVersion(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) now() time.Time { return s.Clock.Now() }
