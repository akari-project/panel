// SPDX-License-Identifier: AGPL-3.0-or-later

// Package catalog 管理套餐、价格行与线路组（spec/11 11.1、BIL-01、BIL-04、BIL-15、BIL-21、ACS-05、ACS-06）。
//
//   - 每个写操作在一个事务中完成：锁定所属套餐或线路组的行，比较 If-Match（CONV-28），修改数据，
//     写审计日志（spec/31 CON-09）与 outbox 事件（CONV-22、CONV-34）。
//   - 强 ETag 由版本列生成（spec/31 CON-05）：套餐与线路组的 version 由本包加 1；价格行、线路组关联的变更同样使
//     所属套餐的版本加 1。派生字段（当前权益数、节点数、plan_ids）不参与 ETag。价格行的 ETag 由 updated_at 生成。
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
	"log/slog"
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

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

// Service 提供套餐、价格行与线路组的读写。
type Service struct {
	Pool  *pgxpool.Pool
	Clock clock.Clock
	Log   *slog.Logger
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

// PriceETag 是价格行的强 ETag，由 updated_at 生成。价格行只有 on_sale 可以改变，且只能由真变假一次（BIL-01）。
func PriceETag(p Price) string {
	return `"` + strconv.FormatInt(p.UpdatedAt.UnixMicro(), 36) + `"`
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

// SiteCurrency 返回站点结算货币 site_currency（CONV-08）；站点尚未初始化或取值异常时为 nil
// （不回退为默认值，异常时 warn 日志只记键名）。
func (s *Service) SiteCurrency(ctx context.Context) (*string, error) {
	return s.readCurrency(ctx, sqlc.New(s.Pool))
}

func (s *Service) readCurrency(ctx context.Context, q *sqlc.Queries) (*string, error) {
	raw, err := q.GetSetting(ctx, "site_currency")
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c string
	if json.Unmarshal(raw, &c) != nil || !currencyCode.MatchString(c) {
		s.log().WarnContext(ctx, "catalog: site setting invalid", "key", "site_currency")
		return nil, nil
	}
	return &c, nil
}

// siteCurrency 返回站点结算货币；站点尚未初始化结算货币或取值异常时返回 409 invalid_state（CONV-08）。
func (s *Service) siteCurrency(ctx context.Context, q *sqlc.Queries) (string, error) {
	c, err := s.readCurrency(ctx, q)
	if err != nil {
		return "", err
	}
	if c == nil {
		return "", apierr.InvalidState
	}
	return *c, nil
}

// freePlanID 返回设置 free_plan_id 引用的套餐（spec/03 3.6，BIL-15）；缺键、null 或取值异常时为 nil
// （按不启用处理，异常时 warn 日志只记键名）。
func (s *Service) freePlanID(ctx context.Context, q *sqlc.Queries) (*uuid.UUID, error) {
	raw, err := q.GetSetting(ctx, "free_plan_id")
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v *string
	if json.Unmarshal(raw, &v) != nil {
		s.log().WarnContext(ctx, "catalog: site setting invalid", "key", "free_plan_id")
		return nil, nil
	}
	if v == nil {
		return nil, nil
	}
	id, err := uuid.Parse(*v)
	if err != nil {
		s.log().WarnContext(ctx, "catalog: site setting invalid", "key", "free_plan_id")
		return nil, nil
	}
	return &id, nil
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

func (s *Service) now() time.Time { return s.Clock.Now() }
