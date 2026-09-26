-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 套餐、价格行与线路组（spec/11，M1-04）。版本列 version 生成强 ETag（CONV-13、CONV-28），由应用加 1。
-- “当前权益”指状态为 active、over_quota、suspended 的权益（spec/11 11.1）。

-- name: ListPlans :many
-- 按 (sort, id) 的游标分页（CONV-11）。
SELECT p.id, p.name, p.description, p.tier, p.kind, p.status, p.bytes_per_cycle, p.device_limit, p.speed_limit_mbps,
       p.reset_policy, p.allow_legacy_renew, p.sort, p.version, p.created_at, p.updated_at,
       (SELECT count(*) FROM entitlements e
        WHERE e.plan_id = p.id AND e.status IN ('active','over_quota','suspended'))::int AS active_entitlement_count
FROM plans p
WHERE (sqlc.narg(status)::text IS NULL OR p.status = sqlc.narg(status)::text)
  AND (sqlc.narg(kind)::text IS NULL OR p.kind = sqlc.narg(kind)::text)
  AND (sqlc.narg(cursor_sort)::int IS NULL OR (p.sort, p.id) > (sqlc.narg(cursor_sort)::int, sqlc.narg(cursor_id)::uuid))
ORDER BY p.sort, p.id
LIMIT sqlc.arg(max_rows);

-- name: GetPlan :one
SELECT p.id, p.name, p.description, p.tier, p.kind, p.status, p.bytes_per_cycle, p.device_limit, p.speed_limit_mbps,
       p.reset_policy, p.allow_legacy_renew, p.sort, p.version, p.created_at, p.updated_at,
       (SELECT count(*) FROM entitlements e
        WHERE e.plan_id = p.id AND e.status IN ('active','over_quota','suspended'))::int AS active_entitlement_count
FROM plans p
WHERE p.id = sqlc.arg(id);

-- name: LockPlan :one
SELECT id, name, description, tier, kind, status, bytes_per_cycle, device_limit, speed_limit_mbps,
       reset_policy, allow_legacy_renew, sort, version
FROM plans WHERE id = sqlc.arg(id) FOR UPDATE;

-- name: ListOnSalePlans :many
-- 客户端接口的在售套餐：不含免费套餐与没有在售价格行的套餐，按 (sort, id)，至多 max_rows 项
-- （spec/30 listPlans，spec/11 BIL-15、BIL-21）。
SELECT p.id, p.name, p.description, p.tier, p.kind, p.bytes_per_cycle, p.device_limit, p.speed_limit_mbps, p.reset_policy
FROM plans p
WHERE p.status = 'on_sale' AND p.kind <> 'free'
  AND EXISTS (SELECT 1 FROM plan_prices pp WHERE pp.plan_id = p.id AND pp.on_sale)
ORDER BY p.sort, p.id
LIMIT sqlc.arg(max_rows);

-- name: InsertPlan :one
INSERT INTO plans (name, description, tier, kind, status, bytes_per_cycle, device_limit, speed_limit_mbps,
                   reset_policy, allow_legacy_renew, sort)
VALUES (sqlc.arg(name), sqlc.narg(description), sqlc.arg(tier), sqlc.arg(kind), sqlc.arg(status), sqlc.arg(bytes_per_cycle),
        sqlc.arg(device_limit), sqlc.narg(speed_limit_mbps), sqlc.arg(reset_policy), sqlc.arg(allow_legacy_renew), sqlc.arg(sort))
RETURNING id;

-- name: UpdatePlan :exec
UPDATE plans SET name = sqlc.arg(name), description = sqlc.narg(description), tier = sqlc.arg(tier), kind = sqlc.arg(kind),
       status = sqlc.arg(status), bytes_per_cycle = sqlc.arg(bytes_per_cycle), device_limit = sqlc.arg(device_limit),
       speed_limit_mbps = sqlc.narg(speed_limit_mbps), reset_policy = sqlc.arg(reset_policy),
       allow_legacy_renew = sqlc.arg(allow_legacy_renew), sort = sqlc.arg(sort), version = version + 1
WHERE id = sqlc.arg(id);

-- name: BumpPlanVersion :exec
-- 子资源（价格行、线路组关联）变更时使套餐的 ETag 失效。
UPDATE plans SET version = version + 1 WHERE id = sqlc.arg(id);

-- name: DeletePlan :exec
DELETE FROM plans WHERE id = sqlc.arg(id);

-- name: PlanUsage :one
-- 套餐是否有价格行、是否有权益（任何状态），用于删除、修改类型与改回 draft 的判断。
SELECT EXISTS (SELECT 1 FROM plan_prices pp WHERE pp.plan_id = sqlc.arg(id))::boolean AS has_prices,
       EXISTS (SELECT 1 FROM entitlements e WHERE e.plan_id = sqlc.arg(id))::boolean AS has_entitlements,
       (SELECT count(*) FROM plan_prices pp WHERE pp.plan_id = sqlc.arg(id) AND pp.on_sale)::int AS on_sale_prices;

-- name: FreePlanExists :one
SELECT EXISTS (SELECT 1 FROM plans WHERE kind = 'free' AND id <> sqlc.arg(except_id))::boolean;

-- name: PlanGroupIDs :many
SELECT plan_id, group_id FROM plan_groups WHERE plan_id = ANY(sqlc.arg(plan_ids)::uuid[]) ORDER BY plan_id, group_id;

-- name: InsertPlanGroup :execrows
INSERT INTO plan_groups (plan_id, group_id) VALUES (sqlc.arg(plan_id), sqlc.arg(group_id)) ON CONFLICT DO NOTHING;

-- name: DeletePlanGroup :execrows
DELETE FROM plan_groups WHERE plan_id = sqlc.arg(plan_id) AND group_id = sqlc.arg(group_id);

-- name: OnSalePrices :many
SELECT id, plan_id, period, period_days, amount_minor, currency, on_sale, created_at, updated_at
FROM plan_prices WHERE plan_id = ANY(sqlc.arg(plan_ids)::uuid[]) AND on_sale
ORDER BY plan_id, id;

-- name: ListPlanPrices :many
-- 按 id（UUIDv7，即创建顺序）倒序的游标分页（CONV-11）。
SELECT id, plan_id, period, period_days, amount_minor, currency, on_sale, created_at, updated_at
FROM plan_prices
WHERE plan_id = sqlc.arg(plan_id)
  AND (sqlc.narg(on_sale)::boolean IS NULL OR on_sale = sqlc.narg(on_sale)::boolean)
  AND (sqlc.narg(before)::uuid IS NULL OR id < sqlc.narg(before)::uuid)
ORDER BY id DESC
LIMIT sqlc.arg(max_rows);

-- name: GetPlanPrice :one
SELECT id, plan_id, period, period_days, amount_minor, currency, on_sale, created_at, updated_at
FROM plan_prices WHERE id = sqlc.arg(id) AND plan_id = sqlc.arg(plan_id);

-- name: LockPlanPrice :one
SELECT id, plan_id, period, period_days, amount_minor, currency, on_sale, created_at, updated_at
FROM plan_prices WHERE id = sqlc.arg(id) AND plan_id = sqlc.arg(plan_id) FOR UPDATE;

-- name: InsertPlanPrice :one
INSERT INTO plan_prices (plan_id, period, period_days, amount_minor, currency)
VALUES (sqlc.arg(plan_id), sqlc.arg(period), sqlc.narg(period_days), sqlc.arg(amount_minor), sqlc.arg(currency))
RETURNING id, plan_id, period, period_days, amount_minor, currency, on_sale, created_at, updated_at;

-- name: DiscontinuePlanPrice :one
UPDATE plan_prices SET on_sale = false WHERE id = sqlc.arg(id) AND on_sale
RETURNING id, plan_id, period, period_days, amount_minor, currency, on_sale, created_at, updated_at;

-- name: DiscontinuePeriodPrice :many
-- 停售同一周期的在售行（改价，BIL-01），返回被停售的行。
UPDATE plan_prices SET on_sale = false
WHERE plan_id = sqlc.arg(plan_id) AND period = sqlc.arg(period) AND on_sale
RETURNING id;

-- name: ListLocationGroups :many
-- 按 (created_at, id) 升序的游标分页（CONV-11）；created_at 只用于排序展示，不参与业务判断（CONV-27）。
SELECT g.id, g.name, g.description, g.min_tier, g.version, g.created_at, g.updated_at,
       (SELECT count(*) FROM node_group_members m WHERE m.group_id = g.id)::int AS host_count
FROM location_groups g
WHERE sqlc.narg(cursor_at)::timestamptz IS NULL
   OR (g.created_at, g.id) > (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid)
ORDER BY g.created_at, g.id
LIMIT sqlc.arg(max_rows);

-- name: GetLocationGroup :one
SELECT g.id, g.name, g.description, g.min_tier, g.version, g.created_at, g.updated_at,
       (SELECT count(*) FROM node_group_members m WHERE m.group_id = g.id)::int AS host_count
FROM location_groups g
WHERE g.id = sqlc.arg(id);

-- name: LockLocationGroup :one
SELECT id, name, description, min_tier, version FROM location_groups WHERE id = sqlc.arg(id) FOR UPDATE;

-- name: LocationGroupTiers :many
-- 给定线路组的 min_tier；不存在的 ID 不返回。
SELECT id, min_tier FROM location_groups WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: GroupPlanIDs :many
SELECT group_id, plan_id FROM plan_groups WHERE group_id = ANY(sqlc.arg(group_ids)::uuid[]) ORDER BY group_id, plan_id;

-- name: InsertLocationGroup :one
INSERT INTO location_groups (name, description, min_tier) VALUES (sqlc.arg(name), sqlc.narg(description), sqlc.narg(min_tier))
RETURNING id;

-- name: UpdateLocationGroup :exec
UPDATE location_groups SET name = sqlc.arg(name), description = sqlc.narg(description), min_tier = sqlc.narg(min_tier),
       version = version + 1
WHERE id = sqlc.arg(id);

-- name: DeleteLocationGroup :exec
DELETE FROM location_groups WHERE id = sqlc.arg(id);

-- name: LocationGroupInUse :one
-- 仍被套餐引用或仍有节点的线路组不能删除（ACS-06）。
SELECT (EXISTS (SELECT 1 FROM plan_groups pg WHERE pg.group_id = sqlc.arg(id))
     OR EXISTS (SELECT 1 FROM node_group_members m WHERE m.group_id = sqlc.arg(id)))::boolean;

-- name: CountPlanHolders :one
-- 持有该套餐当前权益的账号数（影响预览，CON-07）。
SELECT count(DISTINCT account_id)::int FROM entitlements
WHERE plan_id = sqlc.arg(plan_id) AND status IN ('active','over_quota','suspended');

-- name: CountGroupHolders :one
-- 通过套餐可访问该线路组的账号数（影响预览，ACS-05，CON-07）：套餐关联该线路组，且
-- only_changed 为真时，在 min_tier 为 old_min 与 new_min 时的可访问性不同；
-- only_changed 为假时，在两者任一之下可访问。
SELECT count(DISTINCT e.account_id)::int
FROM entitlements e
JOIN plans p ON p.id = e.plan_id
JOIN plan_groups pg ON pg.plan_id = p.id AND pg.group_id = sqlc.arg(group_id)
WHERE e.status IN ('active','over_quota','suspended')
  AND CASE WHEN sqlc.arg(only_changed)::boolean
        THEN (sqlc.narg(old_min)::int IS NULL OR p.tier >= sqlc.narg(old_min)::int)
             IS DISTINCT FROM (sqlc.narg(new_min)::int IS NULL OR p.tier >= sqlc.narg(new_min)::int)
        ELSE (sqlc.narg(old_min)::int IS NULL OR p.tier >= sqlc.narg(old_min)::int)
             OR (sqlc.narg(new_min)::int IS NULL OR p.tier >= sqlc.narg(new_min)::int)
      END;

-- name: GroupMemberIDs :many
SELECT node_id FROM node_group_members WHERE group_id = sqlc.arg(group_id);

-- name: ExistingNodeIDs :many
SELECT id FROM nodes WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: CountGroupHosts :one
SELECT count(DISTINCT node_id)::int FROM node_group_members WHERE group_id = ANY(sqlc.arg(group_ids)::uuid[]);

-- name: PlanRegionCounts :many
-- 套餐可访问的地区数：关联且满足 min_tier 的线路组中，节点的 region_code 去重（spec/30 Plan.location_count）。
SELECT pg.plan_id, count(DISTINCT n.region_code)::int AS regions
FROM plan_groups pg
JOIN plans p ON p.id = pg.plan_id
JOIN location_groups g ON g.id = pg.group_id AND (g.min_tier IS NULL OR p.tier >= g.min_tier)
JOIN node_group_members m ON m.group_id = g.id
JOIN nodes n ON n.id = m.node_id
WHERE pg.plan_id = ANY(sqlc.arg(plan_ids)::uuid[])
GROUP BY pg.plan_id;
