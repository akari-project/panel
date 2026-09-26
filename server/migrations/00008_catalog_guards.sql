-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 00006、00007 的加固（spec/11 BIL-01、BIL-15，CONV-21）：
--   - 00007 以 CREATE INDEX CONCURRENTLY IF NOT EXISTS 建立索引：并发建立失败会留下 INVALID 索引，重新执行时被跳过，
--     至多一个免费套餐（BIL-15）就不再由数据库保证。这里检查两个索引有效，否则中止迁移并给出处理办法；
--   - plan_prices_period_guard 读取套餐类型前以 FOR SHARE 锁定套餐行，使并发修改 kind 的事务不能在检查与插入之间
--     改变类型（plans_kind_guard 在修改 kind 时检查价格行），触发器因此是可靠的兜底。

-- +goose Up
-- +goose StatementBegin
DO $$
DECLARE
  idx text;
BEGIN
  FOREACH idx IN ARRAY ARRAY['plans_single_free', 'plans_sort'] LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
                   WHERE c.relname = idx AND c.relnamespace = 'public'::regnamespace AND i.indisvalid) THEN
      RAISE EXCEPTION 'index % is missing or INVALID (a concurrent build of migration 00007 failed)', idx
        USING HINT = format('Run DROP INDEX CONCURRENTLY IF EXISTS %I; then run the CREATE INDEX CONCURRENTLY statement for it '
                            'from migration 00007 by hand, and run panel migrate again.', idx);
    END IF;
  END LOOP;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION plan_prices_period_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  k text;
BEGIN
  SELECT kind INTO k FROM plans WHERE id = NEW.plan_id FOR SHARE;
  IF (k = 'one_time') <> (NEW.period = 'one_time') THEN
    RAISE EXCEPTION 'plan_prices: period % does not match plan kind %', NEW.period, k USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.period <> 'one_time' AND NEW.period_days IS NOT NULL THEN
    RAISE EXCEPTION 'plan_prices: period_days is only for one_time' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
