-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 初始数据模型。约定见 spec/02-conventions.md，业务规则见 spec/11 与 spec/13。
-- 需要 PostgreSQL 18（uuidv7()）。

-- +goose Up

-- ============ 通用 ============
-- +goose StatementBegin
CREATE FUNCTION forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% is append-only', TG_TABLE_NAME;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION touch_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TABLE settings (
  key         text PRIMARY KEY,
  value       jsonb NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now()
);

-- ============ 账号与认证 ============
CREATE TABLE accounts (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  email              text NOT NULL,
  password_hash      text,
  status             text NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended','deleting','deleted')),
  email_verified_at  timestamptz,
  locale             text NOT NULL DEFAULT 'zh-CN',
  timezone           text,
  referral_code      text NOT NULL UNIQUE,
  referred_by        uuid REFERENCES accounts(id),
  auto_renew         boolean NOT NULL DEFAULT false,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX accounts_email_uq ON accounts (lower(email));
CREATE TRIGGER accounts_touch BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE roles (
  name         text PRIMARY KEY,           -- superadmin / operator / support
  permissions  text[] NOT NULL
);
CREATE TABLE account_roles (
  account_id  uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  role        text NOT NULL REFERENCES roles(name),
  PRIMARY KEY (account_id, role)
);

CREATE TABLE mfa_totp (
  account_id      uuid PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  secret_enc      bytea NOT NULL,
  recovery_hashes text[] NOT NULL,
  enabled_at      timestamptz
);
CREATE TABLE mfa_webauthn (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id     uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  credential_id  bytea NOT NULL UNIQUE,
  public_key     bytea NOT NULL,
  sign_count     bigint NOT NULL DEFAULT 0,
  name           text NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE verification_codes (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid REFERENCES accounts(id) ON DELETE CASCADE,
  purpose      text NOT NULL CHECK (purpose IN ('email_verify','password_reset','email_change')),
  code_hash    text NOT NULL,
  attempts     int NOT NULL DEFAULT 0,
  expires_at   timestamptz NOT NULL,
  consumed_at  timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE devices (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id    uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  platform      text NOT NULL,                 -- ios / android / windows / macos / linux / web / other
  model         text,
  app_version   text,
  public_key    bytea,
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz,
  revoked_at    timestamptz
);
CREATE INDEX devices_account_active ON devices (account_id) WHERE revoked_at IS NULL;

CREATE TABLE sessions (
  id                  uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id          uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  device_id           uuid REFERENCES devices(id) ON DELETE CASCADE,
  refresh_token_hash  text NOT NULL UNIQUE,
  parent_id           uuid REFERENCES sessions(id),   -- 刷新令牌轮换链
  user_agent          text,
  created_at          timestamptz NOT NULL DEFAULT now(),
  expires_at          timestamptz NOT NULL,
  used_at             timestamptz,                    -- 已被轮换；再次使用即判定为泄露
  revoked_at          timestamptz
);
CREATE INDEX sessions_account ON sessions (account_id) WHERE revoked_at IS NULL;

-- 第三方客户端的配置导出令牌（每账号一个，可重置）
CREATE TABLE export_tokens (
  account_id   uuid PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  token_hash   text NOT NULL UNIQUE,
  rotated_at   timestamptz NOT NULL DEFAULT now()
);

-- 代理凭据：自研客户端每台设备一条；device_id 为空表示第三方导出共用的凭据
CREATE TABLE proxy_credentials (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  device_id    uuid REFERENCES devices(id) ON DELETE CASCADE,
  secret_enc   bytea NOT NULL,                 -- UUID / 密码等，按协议解释
  created_at   timestamptz NOT NULL DEFAULT now(),
  revoked_at   timestamptz
);
CREATE UNIQUE INDEX proxy_credentials_shared_uq ON proxy_credentials (account_id) WHERE device_id IS NULL AND revoked_at IS NULL;
CREATE UNIQUE INDEX proxy_credentials_device_uq ON proxy_credentials (device_id) WHERE device_id IS NOT NULL AND revoked_at IS NULL;

-- ============ 节点 ============
CREATE TABLE location_groups (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  name        text NOT NULL UNIQUE,
  description text,
  min_tier    int,
  created_at  timestamptz NOT NULL DEFAULT now()
);

-- 内核与协议支持矩阵（spec/21“协议与内核”）。静态种子数据，随版本迁移更新；
-- Agent 上报的能力清单用于运行时二次校验。
CREATE TABLE kernels (
  name  text PRIMARY KEY          -- singbox / xray
);
CREATE TABLE kernel_protocols (
  kernel    text NOT NULL REFERENCES kernels(name),
  protocol  text NOT NULL,
  status    text NOT NULL CHECK (status IN ('stable','experimental')),
  PRIMARY KEY (kernel, protocol)
);
INSERT INTO kernels (name) VALUES ('singbox'), ('xray');
INSERT INTO kernel_protocols (kernel, protocol, status) VALUES
  ('singbox','vless','stable'), ('singbox','vmess','stable'), ('singbox','trojan','stable'),
  ('singbox','shadowsocks','stable'), ('singbox','hysteria2','stable'), ('singbox','tuic','stable'),
  ('singbox','anytls','stable'),
  ('xray','vless','stable'), ('xray','vmess','stable'), ('xray','trojan','stable'),
  ('xray','shadowsocks','stable'), ('xray','hysteria2','experimental');


CREATE TABLE machines (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  name               text NOT NULL,
  enroll_token_hash  text,
  enroll_expires_at  timestamptz,
  psk_enc            bytea,
  psk_prev_enc       bytea,
  enrolled_at        timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE nodes (
  id                  uuid PRIMARY KEY DEFAULT uuidv7(),
  machine_id          uuid REFERENCES machines(id) ON DELETE SET NULL,  -- 机器模式
  name                text NOT NULL,
  region_code         text NOT NULL,          -- ISO 3166-1 alpha-2，可带城市后缀
  public_host         text NOT NULL,
  status              text NOT NULL DEFAULT 'pending_enroll'
                      CHECK (status IN ('pending_enroll','pending_config','online','offline','maintenance')),
  traffic_multiplier  numeric(4,2) NOT NULL DEFAULT 1.00 CHECK (traffic_multiplier > 0),
  speed_limit_mbps    int,
  enroll_token_hash   text,
  enroll_expires_at   timestamptz,
  psk_enc             bytea,
  psk_prev_enc        bytea,
  kernel_type         text NOT NULL DEFAULT 'singbox' REFERENCES kernels(name),   -- 由控制面选定并下发
  allow_experimental  boolean NOT NULL DEFAULT false,   -- 允许在该节点启用内核的实验协议
  capabilities        jsonb,                           -- Agent 握手时上报的 Capabilities，运行时校验以此为准
  agent_version       text,
  config_version      bigint NOT NULL DEFAULT 0,
  last_seen_at        timestamptz,
  sort                int NOT NULL DEFAULT 0,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER nodes_touch BEFORE UPDATE ON nodes FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE node_group_members (
  node_id   uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  group_id  uuid NOT NULL REFERENCES location_groups(id) ON DELETE RESTRICT,
  PRIMARY KEY (node_id, group_id)
);
CREATE INDEX node_group_members_group ON node_group_members (group_id);

CREATE TABLE inbounds (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  node_id     uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  protocol    text NOT NULL CHECK (protocol IN ('vless','vmess','trojan','shadowsocks','hysteria2','tuic','anytls')),
  listen_port int NOT NULL CHECK (listen_port BETWEEN 1 AND 65535),
  settings    jsonb NOT NULL,                 -- 传输、TLS、Reality 等；密钥部分存 secrets_enc
  secrets_enc bytea,
  version     bigint NOT NULL DEFAULT 1,
  enabled     boolean NOT NULL DEFAULT true,
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (node_id, listen_port)
);

-- 入站协议必须被节点所选内核支持；实验协议需节点显式允许（spec/21）
-- +goose StatementBegin
CREATE FUNCTION inbound_kernel_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  k text;
  allow_exp boolean;
  st text;
BEGIN
  IF NOT NEW.enabled THEN
    RETURN NEW;
  END IF;
  SELECT kernel_type, allow_experimental INTO k, allow_exp FROM nodes WHERE id = NEW.node_id;
  SELECT status INTO st FROM kernel_protocols WHERE kernel = k AND protocol = NEW.protocol;
  IF st IS NULL THEN
    RAISE EXCEPTION 'kernel % does not support protocol %', k, NEW.protocol USING ERRCODE = 'check_violation';
  ELSIF st = 'experimental' AND NOT allow_exp THEN
    RAISE EXCEPTION 'protocol % is experimental on kernel %', NEW.protocol, k USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER inbounds_kernel_guard BEFORE INSERT OR UPDATE OF protocol, node_id, enabled ON inbounds
  FOR EACH ROW EXECUTE FUNCTION inbound_kernel_guard();

-- 切换节点内核或关闭实验协议时，已启用的入站必须全部仍被支持
-- +goose StatementBegin
CREATE FUNCTION node_kernel_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  bad text;
BEGIN
  SELECT string_agg(i.protocol, ',') INTO bad
  FROM inbounds i
  LEFT JOIN kernel_protocols kp ON kp.kernel = NEW.kernel_type AND kp.protocol = i.protocol
  WHERE i.node_id = NEW.id AND i.enabled
    AND (kp.protocol IS NULL OR (kp.status = 'experimental' AND NOT NEW.allow_experimental));
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'kernel % cannot serve existing inbounds: %', NEW.kernel_type, bad USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER nodes_kernel_guard BEFORE UPDATE OF kernel_type, allow_experimental ON nodes
  FOR EACH ROW EXECUTE FUNCTION node_kernel_guard();

CREATE TABLE node_routes (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  node_id     uuid REFERENCES nodes(id) ON DELETE CASCADE,
  group_id    uuid REFERENCES location_groups(id) ON DELETE CASCADE,
  rules       jsonb NOT NULL,                 -- 自定义路由与出站
  created_at  timestamptz NOT NULL DEFAULT now(),
  CHECK ((node_id IS NULL) <> (group_id IS NULL))
);

-- ============ 套餐与价格 ============
CREATE TABLE plans (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  name               text NOT NULL,
  description        text,
  tier               int NOT NULL CHECK (tier >= 0),       -- 0 保留给免费套餐
  kind               text NOT NULL DEFAULT 'recurring' CHECK (kind IN ('recurring','one_time','free')),
  bytes_per_cycle    bigint NOT NULL CHECK (bytes_per_cycle >= 0),
  device_limit       int NOT NULL CHECK (device_limit > 0),
  speed_limit_mbps   int,
  reset_policy       text NOT NULL DEFAULT 'purchase_anchor'
                     CHECK (reset_policy IN ('purchase_anchor','calendar_month','never')),
  status             text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','on_sale','hidden','archived')),
  allow_legacy_renew boolean NOT NULL DEFAULT true,
  sort               int NOT NULL DEFAULT 0,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER plans_touch BEFORE UPDATE ON plans FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE plan_groups (
  plan_id   uuid NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  group_id  uuid NOT NULL REFERENCES location_groups(id) ON DELETE RESTRICT,
  PRIMARY KEY (plan_id, group_id)
);
CREATE INDEX plan_groups_group ON plan_groups (group_id);

-- 价格行只新增与停售，不修改金额
CREATE TABLE plan_prices (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  plan_id       uuid NOT NULL REFERENCES plans(id) ON DELETE RESTRICT,
  period        text NOT NULL CHECK (period IN ('month','quarter','half_year','year','one_time')),
  period_days   int,                          -- one_time 时为有效天数，可为空表示长期
  amount_minor  bigint NOT NULL CHECK (amount_minor >= 0),
  currency      char(3) NOT NULL,
  on_sale       boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX plan_prices_on_sale_uq ON plan_prices (plan_id, period) WHERE on_sale;

-- +goose StatementBegin
CREATE FUNCTION plan_prices_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.amount_minor <> OLD.amount_minor OR NEW.currency <> OLD.currency
     OR NEW.period <> OLD.period OR NEW.plan_id <> OLD.plan_id THEN
    RAISE EXCEPTION 'plan_prices: price rows are immutable except on_sale';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER plan_prices_immutable BEFORE UPDATE ON plan_prices FOR EACH ROW EXECUTE FUNCTION plan_prices_guard();
CREATE TRIGGER plan_prices_nodelete BEFORE DELETE ON plan_prices FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ============ 权益 ============
CREATE TABLE entitlements (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id         uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  plan_id            uuid NOT NULL REFERENCES plans(id),
  locked_price_id    uuid REFERENCES plan_prices(id),   -- 续费计价依据
  status             text NOT NULL CHECK (status IN ('scheduled','active','over_quota','suspended','ended')),
  starts_at          timestamptz NOT NULL,
  expires_at         timestamptz,                        -- one_time 长期有效时为空
  cycle_index        int NOT NULL DEFAULT 0,
  cycle_start        timestamptz NOT NULL,
  cycle_end          timestamptz,
  bytes_limit        bigint NOT NULL,                    -- 快照
  device_limit       int NOT NULL,                       -- 快照
  speed_limit_mbps   int,                                -- 快照
  reset_policy       text NOT NULL,                      -- 快照
  paid_minor         bigint NOT NULL DEFAULT 0,          -- 本段实付，用于剩余价值计算
  version            int NOT NULL DEFAULT 1,             -- 报价与开通时的乐观锁
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
-- 每个账号至多一个当前权益、至多一个排队中的下一段
CREATE UNIQUE INDEX entitlements_current_uq ON entitlements (account_id) WHERE status IN ('active','over_quota','suspended');
CREATE UNIQUE INDEX entitlements_scheduled_uq ON entitlements (account_id) WHERE status = 'scheduled';
CREATE INDEX entitlements_expiry ON entitlements (expires_at) WHERE status IN ('active','over_quota');
CREATE INDEX entitlements_plan ON entitlements (plan_id) WHERE status IN ('active','over_quota');
CREATE TRIGGER entitlements_touch BEFORE UPDATE ON entitlements FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE entitlement_events (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  entitlement_id  uuid NOT NULL REFERENCES entitlements(id),
  account_id      uuid NOT NULL REFERENCES accounts(id),
  type            text NOT NULL CHECK (type IN ('purchase','renew','upgrade','downgrade_scheduled','downgrade_applied',
                    'downgrade_immediate','cycle_reset','addon','expire','suspend','resume','refund','redeem','admin_adjust')),
  order_id        uuid,
  actor_id        uuid,                          -- 空表示系统
  reason          text,
  diff            jsonb NOT NULL,                -- 变更前后差异
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX entitlement_events_account ON entitlement_events (account_id, created_at);
CREATE TRIGGER entitlement_events_append_only BEFORE UPDATE OR DELETE ON entitlement_events FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE addons (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  entitlement_id  uuid NOT NULL REFERENCES entitlements(id) ON DELETE CASCADE,
  kind            text NOT NULL CHECK (kind IN ('traffic','devices')),
  bytes_total     bigint NOT NULL DEFAULT 0,
  bytes_used      bigint NOT NULL DEFAULT 0,
  device_slots    int NOT NULL DEFAULT 0,
  expires_at      timestamptz,
  order_id        uuid,
  created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE usage_cycles (
  entitlement_id  uuid NOT NULL REFERENCES entitlements(id) ON DELETE CASCADE,
  cycle_index     int NOT NULL,
  account_id      uuid NOT NULL REFERENCES accounts(id),
  bytes_up        bigint NOT NULL DEFAULT 0,
  bytes_down      bigint NOT NULL DEFAULT 0,
  started_at      timestamptz NOT NULL,
  closed_at       timestamptz,
  PRIMARY KEY (entitlement_id, cycle_index)
);

-- ============ 订单与账务 ============
CREATE TABLE coupons (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  code_hash        text NOT NULL UNIQUE,
  kind             text NOT NULL CHECK (kind IN ('percent','fixed')),
  value            bigint NOT NULL CHECK (value > 0),       -- percent 时为基点（10000 = 100%）
  plan_ids         uuid[],                                   -- 空表示全部
  periods          text[],
  order_types      text[],
  max_uses         int,
  max_uses_per_account int NOT NULL DEFAULT 1,
  starts_at        timestamptz,
  ends_at          timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE quotes (
  id                   uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id           uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  order_type           text NOT NULL,
  price_id             uuid NOT NULL REFERENCES plan_prices(id),
  entitlement_id       uuid REFERENCES entitlements(id),
  entitlement_version  int,
  breakdown            jsonb NOT NULL,          -- 原价、剩余价值、优惠、余额抵扣、应付
  amount_due_minor     bigint NOT NULL,
  currency             char(3) NOT NULL,
  coupon_id            uuid REFERENCES coupons(id),
  expires_at           timestamptz NOT NULL,
  created_at           timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE orders (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  number             text NOT NULL UNIQUE,
  account_id         uuid NOT NULL REFERENCES accounts(id),
  type               text NOT NULL CHECK (type IN ('new','renew','upgrade','downgrade_scheduled','downgrade_immediate','addon')),
  quote_id           uuid NOT NULL REFERENCES quotes(id),
  price_id           uuid REFERENCES plan_prices(id),
  amount_due_minor   bigint NOT NULL CHECK (amount_due_minor >= 0),
  credit_used_minor  bigint NOT NULL DEFAULT 0,
  currency           char(3) NOT NULL,
  status             text NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','paid','fulfilled','credited','cancelled','expired','refunded')),
  provider           text CHECK (provider IN ('alipay_f2f','test','credit','manual')),
  provider_txn_id    text,
  payment_payload    jsonb,                                   -- 例如支付宝二维码内容与过期时间
  created_while_active boolean NOT NULL DEFAULT false,   -- 续费临界判定
  expires_at         timestamptz NOT NULL,
  paid_at            timestamptz,
  fulfilled_at       timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX orders_provider_txn_uq ON orders (provider, provider_txn_id) WHERE provider_txn_id IS NOT NULL;
-- 同一账号同时只允许一个未支付的变更类订单
CREATE UNIQUE INDEX orders_one_pending_change ON orders (account_id)
  WHERE status = 'pending' AND type IN ('renew','upgrade','downgrade_scheduled','downgrade_immediate');
CREATE TRIGGER orders_touch BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 支付渠道配置（spec/12）。私钥等敏感字段只存密文
CREATE TABLE payment_providers (
  id           text PRIMARY KEY CHECK (id IN ('alipay_f2f','test')),
  enabled      boolean NOT NULL DEFAULT false,
  config       jsonb NOT NULL DEFAULT '{}',     -- 环境、AppID、签名方式、公钥或证书、通知地址、标题模板、超时
  secrets_enc  bytea,                           -- 应用私钥
  updated_by   uuid,
  updated_at   timestamptz NOT NULL DEFAULT now()
);

-- 支付通知原文，只追加，用于对账与排查
CREATE TABLE payment_notifications (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  provider      text NOT NULL,
  order_id      uuid REFERENCES orders(id),
  out_trade_no  text,
  trade_no      text,
  trade_status  text,
  total_amount  text,
  is_verified   boolean NOT NULL,
  result        text NOT NULL CHECK (result IN ('processed','duplicate','rejected_signature','rejected_app','rejected_amount','order_not_found','ignored_status')),
  raw           jsonb NOT NULL,
  received_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payment_notifications_order ON payment_notifications (order_id, received_at);
CREATE TRIGGER payment_notifications_append_only BEFORE UPDATE OR DELETE ON payment_notifications FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE refunds (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),   -- 同时作为支付宝 out_request_no
  order_id         uuid NOT NULL REFERENCES orders(id),
  amount_minor     bigint NOT NULL CHECK (amount_minor > 0),
  destination      text NOT NULL CHECK (destination IN ('original','credit')),
  status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','succeeded','failed')),
  provider_result  jsonb,
  actor_id         uuid NOT NULL,
  reason           text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  completed_at     timestamptz
);

CREATE TABLE coupon_redemptions (
  coupon_id   uuid NOT NULL REFERENCES coupons(id),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  order_id    uuid NOT NULL REFERENCES orders(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (coupon_id, order_id)
);

CREATE TABLE credit_ledger (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id    uuid NOT NULL REFERENCES accounts(id),
  amount_minor  bigint NOT NULL CHECK (amount_minor <> 0),   -- 正数入账，负数支出
  currency      char(3) NOT NULL,
  reason        text NOT NULL CHECK (reason IN ('proration_refund','order_payment','refund','referral','redeem','admin_adjust','stale_quote')),
  order_id      uuid REFERENCES orders(id),
  actor_id      uuid,
  note          text,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX credit_ledger_account ON credit_ledger (account_id, created_at);
CREATE TRIGGER credit_ledger_append_only BEFORE UPDATE OR DELETE ON credit_ledger FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE VIEW account_balances AS
  SELECT account_id, currency, sum(amount_minor)::bigint AS balance_minor
  FROM credit_ledger GROUP BY account_id, currency;

CREATE TABLE redeem_codes (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  code_hash     text NOT NULL UNIQUE,
  kind          text NOT NULL CHECK (kind IN ('credit','plan')),
  amount_minor  bigint,
  price_id      uuid REFERENCES plan_prices(id),
  max_uses      int NOT NULL DEFAULT 1,
  used_count    int NOT NULL DEFAULT 0,
  expires_at    timestamptz,
  batch         text,
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE referral_earnings (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  referrer_id       uuid NOT NULL REFERENCES accounts(id),
  referee_id        uuid NOT NULL REFERENCES accounts(id),
  order_id          uuid NOT NULL UNIQUE REFERENCES orders(id),
  amount_minor      bigint NOT NULL,
  currency          char(3) NOT NULL,
  status            text NOT NULL DEFAULT 'frozen' CHECK (status IN ('frozen','released','voided','review')),
  release_at        timestamptz NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now()
);

-- ============ 流量 ============
CREATE TABLE ingest_batches (
  id           uuid PRIMARY KEY,
  rows         int NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE traffic_hourly (
  hour         timestamptz NOT NULL,
  account_id   uuid NOT NULL,
  node_id      uuid NOT NULL,
  bytes_up     bigint NOT NULL DEFAULT 0,     -- 已按节点倍率换算后的计费字节
  bytes_down   bigint NOT NULL DEFAULT 0,
  raw_up       bigint NOT NULL DEFAULT 0,     -- 原始字节
  raw_down     bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (hour, account_id, node_id)
) PARTITION BY RANGE (hour);
CREATE TABLE traffic_hourly_default PARTITION OF traffic_hourly DEFAULT;
-- 月分区由 worker 预建（始终保持未来 2 个月），见 spec/22

CREATE TABLE traffic_daily (
  day          date NOT NULL,
  account_id   uuid NOT NULL,
  node_id      uuid NOT NULL,
  bytes_up     bigint NOT NULL,
  bytes_down   bigint NOT NULL,
  PRIMARY KEY (day, account_id, node_id)
);

-- ============ 业务模块 ============
CREATE TABLE announcements (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  title       jsonb NOT NULL,                 -- {"zh-CN": "...", "en": "..."}
  body_md     jsonb NOT NULL,
  audience    text NOT NULL DEFAULT 'all' CHECK (audience IN ('all','active','plans')),
  plan_ids    uuid[],
  pinned      boolean NOT NULL DEFAULT false,
  starts_at   timestamptz NOT NULL DEFAULT now(),
  ends_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE support_tickets (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  number      text NOT NULL UNIQUE,
  account_id  uuid NOT NULL REFERENCES accounts(id),
  subject     text NOT NULL,
  category    text NOT NULL,
  status      text NOT NULL DEFAULT 'open' CHECK (status IN ('open','in_progress','waiting_user','closed')),
  assignee_id uuid REFERENCES accounts(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER support_tickets_touch BEFORE UPDATE ON support_tickets FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE TABLE support_messages (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  ticket_id   uuid NOT NULL REFERENCES support_tickets(id) ON DELETE CASCADE,
  author_id   uuid NOT NULL REFERENCES accounts(id),
  body        text NOT NULL,
  attachments jsonb NOT NULL DEFAULT '[]',
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE articles (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  slug        text NOT NULL UNIQUE,
  category    text NOT NULL,
  platforms   text[] NOT NULL DEFAULT '{}',
  title       jsonb NOT NULL,
  body_md     jsonb NOT NULL,
  published   boolean NOT NULL DEFAULT false,
  sort        int NOT NULL DEFAULT 0,
  updated_at  timestamptz NOT NULL DEFAULT now()
);

-- ============ 基础设施 ============
CREATE TABLE outbox (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  topic           text NOT NULL,
  payload         jsonb NOT NULL,
  schema_version  int NOT NULL DEFAULT 1,
  created_at      timestamptz NOT NULL DEFAULT now(),
  published_at    timestamptz
);
CREATE INDEX outbox_unpublished ON outbox (created_at) WHERE published_at IS NULL;

CREATE TABLE notification_outbox (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id    uuid REFERENCES accounts(id) ON DELETE CASCADE,
  channel       text NOT NULL,                -- email / telegram / webhook
  template      text NOT NULL,
  locale        text NOT NULL,
  variables     jsonb NOT NULL,
  attempts      int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  sent_at       timestamptz,
  last_error    text,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notification_outbox_due ON notification_outbox (next_attempt_at) WHERE sent_at IS NULL;

CREATE TABLE idempotency_keys (
  key           uuid NOT NULL,
  account_id    uuid NOT NULL,
  route         text NOT NULL,
  response      jsonb NOT NULL,
  status        int NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, key)
);

CREATE TABLE audit_logs (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  actor_id     uuid,
  action       text NOT NULL,
  target_type  text NOT NULL,
  target_id    text,
  diff         jsonb,
  ip_prefix    text,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_target ON audit_logs (target_type, target_id, created_at);
CREATE TRIGGER audit_logs_append_only BEFORE UPDATE OR DELETE ON audit_logs FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

INSERT INTO roles (name, permissions) VALUES
  ('superadmin', ARRAY['*']),
  ('operator',   ARRAY['nodes.*','plans.*','accounts.read','accounts.adjust','orders.read','announcements.*','articles.*']),
  ('support',    ARRAY['accounts.read','orders.read','tickets.*']);

-- +goose Down
DROP SCHEMA public CASCADE;
CREATE SCHEMA public;
