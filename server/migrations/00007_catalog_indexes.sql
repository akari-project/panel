-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 套餐的索引（spec/11，M1-04）。表中可能已有数据，按 CONV-21 使用 CONCURRENTLY，不在事务中执行：
--   - 至多一个免费套餐（BIL-15）；
--   - 套餐列表按 (sort, id) 的游标分页（spec/31 listPlans，CONV-11）。

-- +goose NO TRANSACTION
-- +goose Up
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS plans_single_free ON plans (kind) WHERE kind = 'free';
CREATE INDEX CONCURRENTLY IF NOT EXISTS plans_sort ON plans (sort, id);
