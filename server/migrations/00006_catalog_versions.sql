-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 套餐、价格、线路组（spec/11，M1-04）：
--   - plans.version、location_groups.version：强 ETag 的版本列（CONV-13、CONV-28），由应用在同一事务中加 1。
--     子资源（价格行、线路组关联）的变更同样使所属套餐的版本加 1；
--   - 价格行的周期必须与套餐类型一致：recurring 只用 month、quarter、half_year、year，one_time 只用 one_time，
--     period_days 只用于 one_time（BIL-01、BIL-09）；
--   - 已有价格行的套餐不能修改类型（取代 00001 中只禁止改为 free 的 plans_kind_guard）。
-- 只加带默认值的非空列（不重写表）、函数与触发器，可以在线执行（DEP-12）。索引见 00007。

-- +goose Up
ALTER TABLE plans ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version > 0);
ALTER TABLE location_groups ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version > 0);

-- +goose StatementBegin
CREATE FUNCTION plan_prices_period_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  k text;
BEGIN
  SELECT kind INTO k FROM plans WHERE id = NEW.plan_id;
  IF (k = 'one_time') <> (NEW.period = 'one_time') THEN
    RAISE EXCEPTION 'plan_prices: period % does not match plan kind %', NEW.period, k USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.period <> 'one_time' AND NEW.period_days IS NOT NULL THEN
    RAISE EXCEPTION 'plan_prices: period_days is only for one_time' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER plan_prices_period_guard BEFORE INSERT ON plan_prices FOR EACH ROW EXECUTE FUNCTION plan_prices_period_guard();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION plans_kind_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.kind IS DISTINCT FROM OLD.kind AND EXISTS (SELECT 1 FROM plan_prices WHERE plan_id = NEW.id) THEN
    RAISE EXCEPTION 'plans: a plan with price rows cannot change kind' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
