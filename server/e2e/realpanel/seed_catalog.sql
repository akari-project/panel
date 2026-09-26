-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 开发与端到端测试用的套餐目录（M1-04）：1 个免费套餐、2 个在售付费套餐（含价格行）、2 个线路组。
-- 可重复执行（固定 ID，ON CONFLICT DO NOTHING）。也可以直接用于本地开发库：
--   psql "$PANEL_DATABASE_URL" -f server/e2e/realpanel/seed_catalog.sql
-- 站点初始化（M1-09）之前，由这里写入结算货币 site_currency = "CNY" 与站点时区 site_timezone = "Asia/Shanghai"
-- （CONV-08、CONV-26，已设定时不变）。免费套餐不设置 free_plan_id，即不启用（BIL-15）。

INSERT INTO settings (key, value) VALUES ('site_currency', '"CNY"'), ('site_timezone', '"Asia/Shanghai"')
ON CONFLICT (key) DO NOTHING;

INSERT INTO location_groups (id, name, description, min_tier) VALUES
  ('0192f0c4-1a00-7000-8000-00000000b001', '亚太标准', '香港、日本、新加坡', NULL),
  ('0192f0c4-1a00-7000-8000-00000000b002', '全球高级', '美国、欧洲，专业版及以上', 2)
ON CONFLICT (id) DO NOTHING;

INSERT INTO plans (id, name, description, tier, kind, status, bytes_per_cycle, device_limit, speed_limit_mbps, reset_policy, sort) VALUES
  ('0192f0c4-1a00-7000-8000-00000000a000', '免费版', '每月 10 GiB', 0, 'free', 'draft', 10737418240, 1, 50, 'calendar_month', 0),
  ('0192f0c4-1a00-7000-8000-00000000a001', '标准版', '日常使用，每月 200 GiB', 1, 'recurring', 'draft', 214748364800, 3, NULL, 'purchase_anchor', 10),
  ('0192f0c4-1a00-7000-8000-00000000a002', '专业版', '全部线路，每月 1 TiB', 2, 'recurring', 'draft', 1099511627776, 5, NULL, 'purchase_anchor', 20)
ON CONFLICT (id) DO NOTHING;

INSERT INTO plan_groups (plan_id, group_id) VALUES
  ('0192f0c4-1a00-7000-8000-00000000a000', '0192f0c4-1a00-7000-8000-00000000b001'),
  ('0192f0c4-1a00-7000-8000-00000000a001', '0192f0c4-1a00-7000-8000-00000000b001'),
  ('0192f0c4-1a00-7000-8000-00000000a002', '0192f0c4-1a00-7000-8000-00000000b001'),
  ('0192f0c4-1a00-7000-8000-00000000a002', '0192f0c4-1a00-7000-8000-00000000b002')
ON CONFLICT DO NOTHING;

INSERT INTO plan_prices (id, plan_id, period, amount_minor, currency) VALUES
  ('0192f0c4-1a00-7000-8000-00000000c001', '0192f0c4-1a00-7000-8000-00000000a001', 'month', 3000, 'CNY'),
  ('0192f0c4-1a00-7000-8000-00000000c002', '0192f0c4-1a00-7000-8000-00000000a001', 'year', 30000, 'CNY'),
  ('0192f0c4-1a00-7000-8000-00000000c003', '0192f0c4-1a00-7000-8000-00000000a002', 'month', 6000, 'CNY'),
  ('0192f0c4-1a00-7000-8000-00000000c004', '0192f0c4-1a00-7000-8000-00000000a002', 'quarter', 16800, 'CNY')
ON CONFLICT (id) DO NOTHING;

UPDATE plans SET status = 'on_sale'
WHERE id IN ('0192f0c4-1a00-7000-8000-00000000a001', '0192f0c4-1a00-7000-8000-00000000a002') AND status = 'draft';
