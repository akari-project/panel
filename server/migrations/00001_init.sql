-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 初始数据模型。约定见 spec/02-conventions.md，表的分组与约束意图见 spec/03，业务规则见 spec/10–13、spec/20–22。
-- 需要 PostgreSQL 18（uuidv7()）。迁移只前进，没有 Down 段（CONV-21）。
--
-- 表的类别（CONV-17、CONV-18）：
--   只追加表：entitlement_events、credit_ledger、audit_logs、payment_notifications，
--             行级触发器禁止 UPDATE 与 DELETE，语句级触发器禁止 TRUNCATE；
--   分区明细表：traffic_hourly；
--   纯关联表：account_roles、node_group_members、plan_groups、coupon_redemptions、consumed_events；
--   其余为可变表，带 updated_at，由触发器 touch_updated_at 维护。
-- 参与业务判断的时间列没有数据库默认值，由应用用注入的时钟写入（CONV-27）。
-- 只追加表不保存自由文本，原因文本放在可变表 reason_texts，以 reason_id 引用（CONV-29）。

-- +goose Up

-- ============ 通用 ============
-- +goose StatementBegin
CREATE FUNCTION forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '%: % is not allowed', TG_TABLE_NAME, TG_OP USING ERRCODE = 'restrict_violation';
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
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER settings_touch BEFORE UPDATE ON settings FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 站点时区（site_timezone，CONV-26）与结算货币（site_currency，ISO 4217，CONV-08）在初始化时设定，之后只读
-- +goose StatementBegin
CREATE FUNCTION settings_readonly_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.key IN ('site_timezone','site_currency') AND (TG_OP = 'DELETE' OR NEW.value IS DISTINCT FROM OLD.value OR NEW.key IS DISTINCT FROM OLD.key) THEN
    RAISE EXCEPTION 'settings: % is read-only after initialization', OLD.key USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER settings_readonly BEFORE UPDATE OR DELETE ON settings FOR EACH ROW EXECUTE FUNCTION settings_readonly_guard();

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
  referrer_id        uuid REFERENCES accounts(id),
  auto_renew         boolean NOT NULL DEFAULT false,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX accounts_email_uq ON accounts (lower(email));
CREATE TRIGGER accounts_touch BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 操作原因的自由文本（CONV-29）。只追加表不保存自由文本，改为以 reason_id 引用本表。
-- account_id 为原因所涉及的账号；删除该账号个人数据时清空其 body（置为空串），行保留以维持引用。
CREATE TABLE reason_texts (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid REFERENCES accounts(id),
  body        text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX reason_texts_account ON reason_texts (account_id) WHERE account_id IS NOT NULL;
CREATE TRIGGER reason_texts_touch BEFORE UPDATE ON reason_texts FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 权限字符串见 spec/10 AUTH-17；内置角色不可修改、不可删除（AUTH-22），由应用层保证
CREATE TABLE roles (
  name         text PRIMARY KEY,
  permissions  text[] NOT NULL,
  is_builtin   boolean NOT NULL DEFAULT false,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER roles_touch BEFORE UPDATE ON roles FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE account_roles (
  account_id  uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  role        text NOT NULL REFERENCES roles(name),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, role)
);
CREATE INDEX account_roles_role ON account_roles (role);

-- 管理员邀请：一次性链接，72 小时有效（AUTH-22）
CREATE TABLE staff_invitations (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  email        text NOT NULL,
  role         text NOT NULL REFERENCES roles(name),
  token_hash   text NOT NULL UNIQUE,
  inviter_id   uuid NOT NULL REFERENCES accounts(id),
  expires_at   timestamptz NOT NULL,
  accepted_at  timestamptz,
  account_id   uuid REFERENCES accounts(id) ON DELETE SET NULL,   -- 接受邀请后建立的账号
  revoked_at   timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER staff_invitations_touch BEFORE UPDATE ON staff_invitations FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE mfa_totp (
  account_id      uuid PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  secret_enc      bytea NOT NULL,
  recovery_hashes text[] NOT NULL,
  last_used_step  bigint,                     -- 同一时间步内已使用的码拒绝再次使用（AUTH-11）
  enabled_at      timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER mfa_totp_touch BEFORE UPDATE ON mfa_totp FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE mfa_webauthn (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id     uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  credential_id  bytea NOT NULL UNIQUE,
  public_key     bytea NOT NULL,
  sign_count     bigint NOT NULL DEFAULT 0,
  name           text NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mfa_webauthn_account ON mfa_webauthn (account_id);
CREATE TRIGGER mfa_webauthn_touch BEFORE UPDATE ON mfa_webauthn FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE verification_codes (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid REFERENCES accounts(id) ON DELETE CASCADE,
  purpose      text NOT NULL CHECK (purpose IN ('email_verify','password_reset','email_change')),
  code_hash    text NOT NULL,
  attempts     int NOT NULL DEFAULT 0,
  expires_at   timestamptz NOT NULL,
  consumed_at  timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX verification_codes_account ON verification_codes (account_id, purpose) WHERE consumed_at IS NULL;
CREATE TRIGGER verification_codes_touch BEFORE UPDATE ON verification_codes FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE devices (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id    uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  platform      text NOT NULL CHECK (platform IN ('ios','android','windows','macos','linux','web','other')),
  model         text,
  app_version   text,
  public_key    bytea,                          -- Ed25519（AUTH-10）
  last_seen_at  timestamptz,
  revoked_at    timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX devices_account_active ON devices (account_id) WHERE revoked_at IS NULL;
CREATE TRIGGER devices_touch BEFORE UPDATE ON devices FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE sessions (
  id                  uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id          uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  device_id           uuid REFERENCES devices(id) ON DELETE CASCADE,
  audience            text NOT NULL CHECK (audience IN ('client','console')),   -- AUTH-06、AUTH-21
  refresh_token_hash  text NOT NULL UNIQUE,
  parent_id           uuid REFERENCES sessions(id),   -- 刷新令牌轮换链
  user_agent          text,
  ip_prefix           text,                           -- /24 或 /48（CONV-24），用于 AUTH-07 的重试判定
  expires_at          timestamptz NOT NULL,
  used_at             timestamptz,                    -- 已被轮换；再次使用即判定为泄露
  revoked_at          timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_account ON sessions (account_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_parent ON sessions (parent_id);
CREATE TRIGGER sessions_touch BEFORE UPDATE ON sessions FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 第三方客户端的配置导出令牌（每账号一个，可重置）。token_hash 用于查找，token_enc 用于再次显示（CONV-20 例外）
CREATE TABLE export_tokens (
  account_id   uuid PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  token_hash   text NOT NULL UNIQUE,
  token_enc    bytea NOT NULL,
  rotated_at   timestamptz NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER export_tokens_touch BEFORE UPDATE ON export_tokens FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 代理凭据：自研客户端每台非 web 设备一条；device_id 为空表示第三方导出共用的凭据（AUTH-13）
CREATE TABLE proxy_credentials (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  device_id    uuid REFERENCES devices(id) ON DELETE CASCADE,
  secret_enc   bytea NOT NULL,                 -- UUID / 密码等，按协议解释
  revoked_at   timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX proxy_credentials_shared_uq ON proxy_credentials (account_id) WHERE device_id IS NULL AND revoked_at IS NULL;
CREATE UNIQUE INDEX proxy_credentials_device_uq ON proxy_credentials (device_id) WHERE device_id IS NOT NULL AND revoked_at IS NULL;
CREATE TRIGGER proxy_credentials_touch BEFORE UPDATE ON proxy_credentials FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ============ 节点 ============
CREATE TABLE location_groups (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  name        text NOT NULL UNIQUE,
  description text,
  min_tier    int CHECK (min_tier >= 0),       -- ACS-05
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER location_groups_touch BEFORE UPDATE ON location_groups FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 内核与协议、传输支持矩阵（spec/21 21.2）。静态基线，随内核升级在新迁移中更新；
-- Agent 上报的能力清单用于运行时二次校验（AGT-09）。
CREATE TABLE kernels (
  name        text PRIMARY KEY,          -- singbox / xray
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER kernels_touch BEFORE UPDATE ON kernels FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE kernel_protocols (
  kernel      text NOT NULL REFERENCES kernels(name),
  protocol    text NOT NULL,
  status      text NOT NULL CHECK (status IN ('stable','experimental')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (kernel, protocol)
);
CREATE TRIGGER kernel_protocols_touch BEFORE UPDATE ON kernel_protocols FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 传输名与 proto 枚举 Transport 的小写形式一致；入站以 settings->>'transport' 为准（AGT-14）
CREATE TABLE kernel_transports (
  kernel      text NOT NULL REFERENCES kernels(name),
  transport   text NOT NULL,
  status      text NOT NULL CHECK (status IN ('stable','experimental')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (kernel, transport)
);
CREATE TRIGGER kernel_transports_touch BEFORE UPDATE ON kernel_transports FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

INSERT INTO kernels (name) VALUES ('singbox'), ('xray');
INSERT INTO kernel_protocols (kernel, protocol, status) VALUES
  ('singbox','vless','stable'), ('singbox','vmess','stable'), ('singbox','trojan','stable'),
  ('singbox','shadowsocks','stable'), ('singbox','hysteria2','stable'), ('singbox','tuic','stable'),
  ('singbox','anytls','stable'),
  ('xray','vless','stable'), ('xray','vmess','stable'), ('xray','trojan','stable'),
  ('xray','shadowsocks','stable'), ('xray','hysteria2','experimental');
INSERT INTO kernel_transports (kernel, transport, status) VALUES
  ('singbox','tcp','stable'), ('singbox','ws','stable'), ('singbox','grpc','stable'),
  ('singbox','httpupgrade','stable'), ('singbox','quic','stable'),
  ('xray','tcp','stable'), ('xray','ws','stable'), ('xray','grpc','stable'),
  ('xray','httpupgrade','stable'), ('xray','xhttp','stable'), ('xray','mkcp','stable'),
  ('xray','quic','experimental');

CREATE TABLE machines (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  name               text NOT NULL,
  enroll_token_hash  text,
  enroll_expires_at  timestamptz,
  psk_enc            bytea,
  psk_prev_enc       bytea,
  enrolled_at        timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER machines_touch BEFORE UPDATE ON machines FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE nodes (
  id                  uuid PRIMARY KEY DEFAULT uuidv7(),
  machine_id          uuid REFERENCES machines(id) ON DELETE SET NULL,  -- 机器模式
  name                text NOT NULL,
  region_code         text NOT NULL,          -- ISO 3166-1 alpha-2，可带城市后缀
  public_host         text NOT NULL,
  status              text NOT NULL DEFAULT 'pending_enroll'
                      CHECK (status IN ('pending_enroll','pending_config','online','offline','maintenance')),
  traffic_multiplier  numeric(4,2) NOT NULL DEFAULT 1.00 CHECK (traffic_multiplier > 0),
  speed_limit_mbps    int CHECK (speed_limit_mbps > 0),
  enroll_token_hash   text,
  enroll_expires_at   timestamptz,
  psk_enc             bytea,
  psk_prev_enc        bytea,
  kernel_type         text NOT NULL DEFAULT 'singbox' REFERENCES kernels(name),   -- 由控制面选定并下发
  allow_experimental  boolean NOT NULL DEFAULT false,   -- 允许在该节点启用内核的实验协议与传输（AGT-10）
  capabilities        jsonb,                           -- Agent 握手时上报的 Capabilities，运行时校验以此为准
  agent_version       text,
  config_version      bigint NOT NULL DEFAULT 0,
  last_report_seq     bigint NOT NULL DEFAULT 0,       -- 已入账的最大 report_seq（ACC-03）
  last_seen_at        timestamptz,
  sort                int NOT NULL DEFAULT 0,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX nodes_enroll_token_uq ON nodes (enroll_token_hash) WHERE enroll_token_hash IS NOT NULL;
CREATE TRIGGER nodes_touch BEFORE UPDATE ON nodes FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE node_group_members (
  node_id     uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  group_id    uuid NOT NULL REFERENCES location_groups(id) ON DELETE RESTRICT,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (node_id, group_id)
);
CREATE INDEX node_group_members_group ON node_group_members (group_id);

CREATE TABLE inbounds (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  node_id     uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  protocol    text NOT NULL CHECK (protocol IN ('vless','vmess','trojan','shadowsocks','hysteria2','tuic','anytls')),
  listen_port int NOT NULL CHECK (listen_port BETWEEN 1 AND 65535),
  settings    jsonb NOT NULL CHECK (jsonb_typeof(settings) = 'object' AND settings ? 'transport'),   -- 非敏感配置（AGT-13、AGT-14）
  secrets_enc bytea,                          -- 私钥（Reality private_key、Shadowsocks 2022 服务端密钥，CONV-19）
  version     bigint NOT NULL DEFAULT 1,
  enabled     boolean NOT NULL DEFAULT true,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (node_id, listen_port)
);
CREATE TRIGGER inbounds_touch BEFORE UPDATE ON inbounds FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 已启用入站的协议与传输必须被节点所选内核支持；实验协议或传输需节点显式允许（AGT-09、AGT-10）
-- +goose StatementBegin
CREATE FUNCTION inbound_kernel_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  k text;
  allow_exp boolean;
  tr text := NEW.settings->>'transport';
  pst text;
  tst text;
BEGIN
  IF NOT NEW.enabled THEN
    RETURN NEW;
  END IF;
  SELECT kernel_type, allow_experimental INTO k, allow_exp FROM nodes WHERE id = NEW.node_id;
  SELECT status INTO pst FROM kernel_protocols WHERE kernel = k AND protocol = NEW.protocol;
  SELECT status INTO tst FROM kernel_transports WHERE kernel = k AND transport = tr;
  IF pst IS NULL THEN
    RAISE EXCEPTION 'kernel % does not support protocol %', k, NEW.protocol USING ERRCODE = 'check_violation';
  ELSIF tst IS NULL THEN
    RAISE EXCEPTION 'kernel % does not support transport %', k, coalesce(tr, '(none)') USING ERRCODE = 'check_violation';
  ELSIF (pst = 'experimental' OR tst = 'experimental') AND NOT allow_exp THEN
    RAISE EXCEPTION 'protocol % over % is experimental on kernel %', NEW.protocol, tr, k USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER inbounds_kernel_guard BEFORE INSERT OR UPDATE OF protocol, node_id, enabled, settings ON inbounds
  FOR EACH ROW EXECUTE FUNCTION inbound_kernel_guard();

-- 切换节点内核或关闭实验开关时，已启用的入站必须全部仍被支持
-- +goose StatementBegin
CREATE FUNCTION node_kernel_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  bad text;
BEGIN
  SELECT string_agg(i.protocol || '/' || coalesce(i.settings->>'transport', '(none)'), ',') INTO bad
  FROM inbounds i
  LEFT JOIN kernel_protocols kp ON kp.kernel = NEW.kernel_type AND kp.protocol = i.protocol
  LEFT JOIN kernel_transports kt ON kt.kernel = NEW.kernel_type AND kt.transport = i.settings->>'transport'
  WHERE i.node_id = NEW.id AND i.enabled
    AND (kp.protocol IS NULL OR kt.transport IS NULL
         OR ((kp.status = 'experimental' OR kt.status = 'experimental') AND NOT NEW.allow_experimental));
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
  updated_at  timestamptz NOT NULL DEFAULT now(),
  CHECK ((node_id IS NULL) <> (group_id IS NULL))
);
CREATE TRIGGER node_routes_touch BEFORE UPDATE ON node_routes FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ============ 套餐与价格 ============
CREATE TABLE plans (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  name               text NOT NULL,
  description        text,
  tier               int NOT NULL CHECK (tier >= 0),
  kind               text NOT NULL DEFAULT 'recurring' CHECK (kind IN ('recurring','one_time','free')),
  bytes_per_cycle    bigint NOT NULL CHECK (bytes_per_cycle >= 0),   -- 0 表示不限流量（BIL-11）
  device_limit       int NOT NULL CHECK (device_limit > 0),
  speed_limit_mbps   int CHECK (speed_limit_mbps > 0),
  reset_policy       text NOT NULL DEFAULT 'purchase_anchor'
                     CHECK (reset_policy IN ('purchase_anchor','calendar_month','never')),
  status             text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','on_sale','hidden','archived')),
  allow_legacy_renew boolean NOT NULL DEFAULT true,
  sort               int NOT NULL DEFAULT 0,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CHECK ((kind = 'free') = (tier = 0))       -- tier 0 保留给免费套餐（spec/11 11.1）
);
CREATE TRIGGER plans_touch BEFORE UPDATE ON plans FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE plan_groups (
  plan_id     uuid NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  group_id    uuid NOT NULL REFERENCES location_groups(id) ON DELETE RESTRICT,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (plan_id, group_id)
);
CREATE INDEX plan_groups_group ON plan_groups (group_id);

-- 价格行只新增与停售（BIL-01）
CREATE TABLE plan_prices (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  plan_id       uuid NOT NULL REFERENCES plans(id) ON DELETE RESTRICT,
  period        text NOT NULL CHECK (period IN ('month','quarter','half_year','year','one_time')),
  period_days   int CHECK (period_days > 0),   -- one_time 时为有效天数，可为空表示长期
  amount_minor  bigint NOT NULL CHECK (amount_minor > 0),
  currency      char(3) NOT NULL,
  on_sale       boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX plan_prices_on_sale_uq ON plan_prices (plan_id, period) WHERE on_sale;

-- 金额、币种、周期、period_days 与所属套餐不可修改；on_sale 只能由真变假
-- +goose StatementBegin
CREATE FUNCTION plan_prices_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.amount_minor IS DISTINCT FROM OLD.amount_minor OR NEW.currency IS DISTINCT FROM OLD.currency
     OR NEW.period IS DISTINCT FROM OLD.period OR NEW.period_days IS DISTINCT FROM OLD.period_days
     OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.id IS DISTINCT FROM OLD.id THEN
    RAISE EXCEPTION 'plan_prices: price rows are immutable except on_sale' USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.on_sale AND NOT OLD.on_sale THEN
    RAISE EXCEPTION 'plan_prices: a price row cannot be put back on sale' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER plan_prices_immutable BEFORE UPDATE ON plan_prices FOR EACH ROW EXECUTE FUNCTION plan_prices_guard();
CREATE TRIGGER plan_prices_nodelete BEFORE DELETE ON plan_prices FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER plan_prices_notruncate BEFORE TRUNCATE ON plan_prices FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER plan_prices_touch BEFORE UPDATE ON plan_prices FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 免费套餐不设价格行（BIL-01、BIL-15）；非免费套餐的金额大于 0 由 CHECK 保证
-- +goose StatementBegin
CREATE FUNCTION plan_prices_plan_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  k text;
BEGIN
  SELECT kind INTO k FROM plans WHERE id = NEW.plan_id;
  IF k = 'free' THEN
    RAISE EXCEPTION 'plan_prices: free plans have no price rows' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER plan_prices_plan_guard BEFORE INSERT ON plan_prices FOR EACH ROW EXECUTE FUNCTION plan_prices_plan_guard();

-- 已有价格行的套餐不能改为免费套餐
-- +goose StatementBegin
CREATE FUNCTION plans_kind_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.kind = 'free' AND OLD.kind <> 'free' AND EXISTS (SELECT 1 FROM plan_prices WHERE plan_id = NEW.id) THEN
    RAISE EXCEPTION 'plans: a plan with price rows cannot become free' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER plans_kind_guard BEFORE UPDATE OF kind ON plans FOR EACH ROW EXECUTE FUNCTION plans_kind_guard();

-- 加购项价格（BIL-24），规则同 plan_prices
CREATE TABLE addon_prices (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  kind          text NOT NULL CHECK (kind IN ('traffic','devices')),
  bytes_total   bigint NOT NULL DEFAULT 0 CHECK (bytes_total >= 0),
  device_slots  int NOT NULL DEFAULT 0 CHECK (device_slots >= 0),
  amount_minor  bigint NOT NULL CHECK (amount_minor > 0),
  currency      char(3) NOT NULL,
  on_sale       boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  CHECK ((kind = 'traffic' AND bytes_total > 0 AND device_slots = 0)
      OR (kind = 'devices' AND device_slots > 0 AND bytes_total = 0))
);

-- +goose StatementBegin
CREATE FUNCTION addon_prices_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.amount_minor IS DISTINCT FROM OLD.amount_minor OR NEW.currency IS DISTINCT FROM OLD.currency
     OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.bytes_total IS DISTINCT FROM OLD.bytes_total
     OR NEW.device_slots IS DISTINCT FROM OLD.device_slots OR NEW.id IS DISTINCT FROM OLD.id THEN
    RAISE EXCEPTION 'addon_prices: price rows are immutable except on_sale' USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.on_sale AND NOT OLD.on_sale THEN
    RAISE EXCEPTION 'addon_prices: a price row cannot be put back on sale' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER addon_prices_immutable BEFORE UPDATE ON addon_prices FOR EACH ROW EXECUTE FUNCTION addon_prices_guard();
CREATE TRIGGER addon_prices_nodelete BEFORE DELETE ON addon_prices FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER addon_prices_notruncate BEFORE TRUNCATE ON addon_prices FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER addon_prices_touch BEFORE UPDATE ON addon_prices FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ============ 权益 ============
CREATE TABLE entitlements (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id         uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  plan_id            uuid NOT NULL REFERENCES plans(id),
  locked_price_id    uuid REFERENCES plan_prices(id),   -- 续费计价依据
  status             text NOT NULL CHECK (status IN ('scheduled','active','over_quota','suspended','ended')),
  starts_at          timestamptz NOT NULL,
  expires_at         timestamptz,                        -- one_time 长期有效与免费套餐时为空
  cycle_index        int NOT NULL DEFAULT 0,
  cycle_start        timestamptz NOT NULL,
  cycle_end          timestamptz,
  bytes_limit        bigint NOT NULL,                    -- 快照
  device_limit       int NOT NULL,                       -- 快照
  speed_limit_mbps   int,                                -- 快照
  reset_policy       text NOT NULL CHECK (reset_policy IN ('purchase_anchor','calendar_month','never')),   -- 快照
  paid_minor         bigint NOT NULL DEFAULT 0 CHECK (paid_minor >= 0),   -- 本段实付（BIL-20）
  version            int NOT NULL DEFAULT 1,             -- 每条权益事件加 1（BIL-03）
  ended_at           timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
-- 每个账号至多一个当前权益、至多一个排队中的下一段（BIL-05）
CREATE UNIQUE INDEX entitlements_current_uq ON entitlements (account_id) WHERE status IN ('active','over_quota','suspended');
CREATE UNIQUE INDEX entitlements_scheduled_uq ON entitlements (account_id) WHERE status = 'scheduled';
-- 到期扫描覆盖 active、over_quota、suspended（BIL-13）
CREATE INDEX entitlements_expiry ON entitlements (expires_at) WHERE status IN ('active','over_quota','suspended');
CREATE INDEX entitlements_plan ON entitlements (plan_id) WHERE status IN ('active','over_quota','suspended');
CREATE INDEX entitlements_account ON entitlements (account_id, created_at);
CREATE TRIGGER entitlements_touch BEFORE UPDATE ON entitlements FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE usage_cycles (
  entitlement_id  uuid NOT NULL REFERENCES entitlements(id) ON DELETE CASCADE,
  cycle_index     int NOT NULL,
  account_id      uuid NOT NULL REFERENCES accounts(id),
  bytes_up        bigint NOT NULL DEFAULT 0,
  bytes_down      bigint NOT NULL DEFAULT 0,
  started_at      timestamptz NOT NULL,
  closed_at       timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (entitlement_id, cycle_index)
);
CREATE TRIGGER usage_cycles_touch BEFORE UPDATE ON usage_cycles FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ============ 订单与账务 ============
CREATE TABLE coupons (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  code_hash        text NOT NULL UNIQUE,
  kind             text NOT NULL CHECK (kind IN ('percent','fixed')),
  value            bigint NOT NULL CHECK (value > 0),       -- percent 时为基点（10000 = 100%）
  plan_ids         uuid[],                                   -- 空表示全部
  periods          text[],
  order_types      text[],
  max_uses         int CHECK (max_uses > 0),
  max_uses_per_account int NOT NULL DEFAULT 1 CHECK (max_uses_per_account > 0),
  starts_at        timestamptz,
  ends_at          timestamptz,
  disabled_at      timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  CHECK (kind <> 'percent' OR value <= 10000)               -- ORD-13
);
CREATE TRIGGER coupons_touch BEFORE UPDATE ON coupons FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE quotes (
  id                   uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id           uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  order_type           text NOT NULL CHECK (order_type IN ('new','renew','upgrade','downgrade_scheduled','downgrade_immediate','addon')),
  price_id             uuid REFERENCES plan_prices(id),
  addon_price_id       uuid REFERENCES addon_prices(id),
  entitlement_id       uuid REFERENCES entitlements(id),
  entitlement_version  int,
  breakdown            jsonb NOT NULL,          -- 原价、剩余价值、优惠、余额抵扣、应付、开通后到期时间（ORD-01）
  amount_due_minor     bigint NOT NULL CHECK (amount_due_minor >= 0),
  currency             char(3) NOT NULL,
  coupon_id            uuid REFERENCES coupons(id),
  expires_at           timestamptz NOT NULL,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  CHECK ((order_type = 'addon') = (addon_price_id IS NOT NULL)),
  CHECK ((price_id IS NULL) <> (addon_price_id IS NULL))
);
CREATE TRIGGER quotes_touch BEFORE UPDATE ON quotes FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE orders (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  number             text NOT NULL UNIQUE,                    -- ORD-YYYYMMDD-XXXXXX（CONV-02）
  account_id         uuid NOT NULL REFERENCES accounts(id),
  type               text NOT NULL CHECK (type IN ('new','renew','upgrade','downgrade_scheduled','downgrade_immediate','addon')),
  quote_id           uuid NOT NULL REFERENCES quotes(id),
  price_id           uuid REFERENCES plan_prices(id),
  addon_price_id     uuid REFERENCES addon_prices(id),
  amount_due_minor   bigint NOT NULL CHECK (amount_due_minor >= 0),
  credit_used_minor  bigint NOT NULL DEFAULT 0 CHECK (credit_used_minor >= 0),   -- 下单时冻结的余额抵扣（ORD-15）
  currency           char(3) NOT NULL,
  status             text NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','paid','fulfilled','credited','cancelled','expired','refunded')),
  provider           text CHECK (provider IN ('alipay_f2f','test','credit','manual')),
  provider_txn_id    text,
  payer_ref_hash     text,                                    -- 支付账号标识的 SHA-256（PAY-05、OPS-07）
  payment_payload    jsonb,                                   -- 例如支付宝二维码内容与过期时间
  refunded_minor          bigint NOT NULL DEFAULT 0 CHECK (refunded_minor >= 0),            -- 累计已退金额（ORD-09）
  refunded_original_minor bigint NOT NULL DEFAULT 0 CHECK (refunded_original_minor >= 0),   -- 其中原路退回的部分
  created_while_active boolean NOT NULL DEFAULT false,        -- 临界续费判定（ORD-06）
  expires_at         timestamptz NOT NULL,
  paid_at            timestamptz,
  fulfilled_at       timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CHECK (refunded_original_minor <= refunded_minor),
  CHECK (refunded_minor <= amount_due_minor + credit_used_minor)
);
CREATE UNIQUE INDEX orders_provider_txn_uq ON orders (provider, provider_txn_id) WHERE provider_txn_id IS NOT NULL;
-- 同一账号同时至多一个未支付的变更类订单，以及至多一个未支付的新购订单（ORD-02）
CREATE UNIQUE INDEX orders_one_pending_change ON orders (account_id)
  WHERE status = 'pending' AND type IN ('renew','upgrade','downgrade_scheduled','downgrade_immediate');
CREATE UNIQUE INDEX orders_one_pending_new ON orders (account_id) WHERE status = 'pending' AND type = 'new';
CREATE INDEX orders_account ON orders (account_id, created_at);
CREATE INDEX orders_pending_expiry ON orders (expires_at) WHERE status = 'pending';
CREATE TRIGGER orders_touch BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE entitlement_events (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  entitlement_id  uuid NOT NULL REFERENCES entitlements(id),
  account_id      uuid NOT NULL REFERENCES accounts(id),
  type            text NOT NULL CHECK (type IN ('purchase','renew','upgrade','downgrade_scheduled','downgrade_applied',
                    'downgrade_immediate','quota_exhausted','cycle_reset','addon','expire','suspend','resume','refund',
                    'redeem','scheduled_cancelled','free_grant','admin_adjust')),   -- spec/11 11.6
  order_id        uuid REFERENCES orders(id),
  actor_id        uuid,                          -- 空表示系统
  reason_id       uuid REFERENCES reason_texts(id),   -- 原因文本（CONV-29）
  diff            jsonb NOT NULL,                -- 变更前后差异
  created_at      timestamptz NOT NULL DEFAULT now(),
  CHECK (type <> 'admin_adjust' OR reason_id IS NOT NULL)   -- admin_adjust 必须填写原因（spec/11 11.6）
);
CREATE INDEX entitlement_events_account ON entitlement_events (account_id, created_at);
CREATE INDEX entitlement_events_entitlement ON entitlement_events (entitlement_id, created_at);
CREATE TRIGGER entitlement_events_append_only BEFORE UPDATE OR DELETE ON entitlement_events FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER entitlement_events_no_truncate BEFORE TRUNCATE ON entitlement_events FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();

CREATE TABLE addons (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  entitlement_id  uuid NOT NULL REFERENCES entitlements(id) ON DELETE CASCADE,
  kind            text NOT NULL CHECK (kind IN ('traffic','devices')),
  addon_price_id  uuid REFERENCES addon_prices(id),
  bytes_total     bigint NOT NULL DEFAULT 0 CHECK (bytes_total >= 0),
  bytes_used      bigint NOT NULL DEFAULT 0 CHECK (bytes_used >= 0),
  device_slots    int NOT NULL DEFAULT 0 CHECK (device_slots >= 0),
  paid_minor      bigint NOT NULL DEFAULT 0 CHECK (paid_minor >= 0),   -- 单独退款的依据（BIL-25）
  expires_at      timestamptz,
  order_id        uuid REFERENCES orders(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX addons_entitlement ON addons (entitlement_id, expires_at);
CREATE TRIGGER addons_touch BEFORE UPDATE ON addons FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 支付渠道配置（spec/12）。私钥等敏感字段只存密文
CREATE TABLE payment_providers (
  id           text PRIMARY KEY CHECK (id IN ('alipay_f2f','test')),
  enabled      boolean NOT NULL DEFAULT false,
  config       jsonb NOT NULL DEFAULT '{}',     -- 环境、AppID、签名方式、公钥或证书、通知地址、标题模板、超时
  secrets_enc  bytea,                           -- 应用私钥
  updated_by   uuid REFERENCES accounts(id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER payment_providers_touch BEFORE UPDATE ON payment_providers FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 支付通知原文，只追加，用于对账与排查。写入前去除买家账号类字段（PAY-06、CONV-29）
CREATE TABLE payment_notifications (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  provider      text NOT NULL,
  order_id      uuid REFERENCES orders(id),
  out_trade_no  text,
  trade_no      text,
  trade_status  text,
  total_amount  text,
  is_verified   boolean NOT NULL,
  result        text NOT NULL CHECK (result IN ('processed','duplicate','late_paid','rejected_signature','rejected_app',
                  'rejected_amount','order_not_found','ignored_status')),
  raw           jsonb NOT NULL,
  received_at   timestamptz NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payment_notifications_order ON payment_notifications (order_id, received_at);
CREATE TRIGGER payment_notifications_append_only BEFORE UPDATE OR DELETE ON payment_notifications FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER payment_notifications_no_truncate BEFORE TRUNCATE ON payment_notifications FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();

CREATE TABLE refunds (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),   -- 同时作为支付宝 out_request_no
  order_id         uuid NOT NULL REFERENCES orders(id),
  addon_id         uuid REFERENCES addons(id),          -- 加购项单独退款（BIL-25）
  amount_minor     bigint NOT NULL CHECK (amount_minor > 0),
  destination      text NOT NULL CHECK (destination IN ('original','credit')),
  status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','succeeded','failed')),
  provider_result  jsonb,
  actor_id         uuid NOT NULL REFERENCES accounts(id),
  reason           text NOT NULL,
  completed_at     timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refunds_order ON refunds (order_id);
CREATE TRIGGER refunds_touch BEFORE UPDATE ON refunds FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 优惠券使用记录；订单取消、过期、转入余额或报价失效时删除以归还次数（ORD-13）
CREATE TABLE coupon_redemptions (
  coupon_id   uuid NOT NULL REFERENCES coupons(id),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  order_id    uuid NOT NULL REFERENCES orders(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (coupon_id, order_id)
);
CREATE INDEX coupon_redemptions_account ON coupon_redemptions (coupon_id, account_id);

-- 余额流水，只追加；方向由原因决定（ORD-16）。原因说明写入 audit_logs，不在此保存自由文本（CONV-29）
CREATE TABLE credit_ledger (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id    uuid NOT NULL REFERENCES accounts(id),
  amount_minor  bigint NOT NULL CHECK (amount_minor <> 0),   -- 正数入账，负数支出
  currency      char(3) NOT NULL,
  reason        text NOT NULL CHECK (reason IN ('order_payment','order_payment_release','stale_quote','quote_adjustment',
                  'late_payment','upgrade_surplus','scheduled_cancelled','refund','redeem','referral','admin_adjust',
                  'account_deletion')),
  order_id      uuid REFERENCES orders(id),
  actor_id      uuid,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CHECK (CASE
           WHEN reason IN ('order_payment','account_deletion') THEN amount_minor < 0
           WHEN reason IN ('referral','admin_adjust') THEN true
           ELSE amount_minor > 0
         END)
);
CREATE INDEX credit_ledger_account ON credit_ledger (account_id, created_at);
CREATE TRIGGER credit_ledger_append_only BEFORE UPDATE OR DELETE ON credit_ledger FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER credit_ledger_no_truncate BEFORE TRUNCATE ON credit_ledger FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();
CREATE VIEW account_balances AS
  SELECT account_id, currency, sum(amount_minor)::bigint AS balance_minor
  FROM credit_ledger GROUP BY account_id, currency;

CREATE TABLE redeem_codes (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  code_hash     text NOT NULL UNIQUE,
  kind          text NOT NULL CHECK (kind IN ('credit','plan')),
  amount_minor  bigint CHECK (amount_minor > 0),
  currency      char(3),
  price_id      uuid REFERENCES plan_prices(id),
  max_uses      int NOT NULL DEFAULT 1 CHECK (max_uses > 0),
  used_count    int NOT NULL DEFAULT 0 CHECK (used_count >= 0),
  expires_at    timestamptz,
  batch         text,
  disabled_at   timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  CHECK (used_count <= max_uses),
  CHECK ((kind = 'credit' AND amount_minor IS NOT NULL AND currency IS NOT NULL AND price_id IS NULL)
      OR (kind = 'plan' AND price_id IS NOT NULL AND amount_minor IS NULL AND currency IS NULL))
);
CREATE INDEX redeem_codes_batch ON redeem_codes (batch);
CREATE TRIGGER redeem_codes_touch BEFORE UPDATE ON redeem_codes FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 兑换记录：同一账号对同一兑换码只能兑换一次（ORD-14）
CREATE TABLE redeem_redemptions (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  redeem_code_id    uuid NOT NULL REFERENCES redeem_codes(id),
  account_id        uuid NOT NULL REFERENCES accounts(id),
  credit_ledger_id  uuid REFERENCES credit_ledger(id),
  entitlement_id    uuid REFERENCES entitlements(id),
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (redeem_code_id, account_id),
  CHECK ((credit_ledger_id IS NULL) <> (entitlement_id IS NULL))
);
CREATE TRIGGER redeem_redemptions_touch BEFORE UPDATE ON redeem_redemptions FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE referral_earnings (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  referrer_id       uuid NOT NULL REFERENCES accounts(id),
  referee_id        uuid NOT NULL REFERENCES accounts(id),
  order_id          uuid NOT NULL UNIQUE REFERENCES orders(id),
  amount_minor      bigint NOT NULL CHECK (amount_minor >= 0),
  currency          char(3) NOT NULL,
  status            text NOT NULL DEFAULT 'frozen' CHECK (status IN ('frozen','released','voided','review')),
  release_at        timestamptz NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER referral_earnings_touch BEFORE UPDATE ON referral_earnings FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ============ 流量 ============
CREATE TABLE ingest_batches (
  id           uuid PRIMARY KEY,               -- 落库批次 batch_id（ACC-05）
  rows         int NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER ingest_batches_touch BEFORE UPDATE ON ingest_batches FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE traffic_hourly (
  hour         timestamptz NOT NULL,
  account_id   uuid NOT NULL,
  node_id      uuid NOT NULL,
  bytes_up     bigint NOT NULL DEFAULT 0,     -- 已按节点倍率换算后的计费字节
  bytes_down   bigint NOT NULL DEFAULT 0,
  raw_up       bigint NOT NULL DEFAULT 0,     -- 原始字节
  raw_down     bigint NOT NULL DEFAULT 0,
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (hour, account_id, node_id)
) PARTITION BY RANGE (hour);
CREATE TABLE traffic_hourly_default PARTITION OF traffic_hourly DEFAULT;
-- 月分区由 worker 预建（始终保持未来 2 个月），见 spec/22 ACC-13

CREATE TABLE traffic_daily (
  day          date NOT NULL,
  account_id   uuid NOT NULL,
  node_id      uuid NOT NULL,
  bytes_up     bigint NOT NULL,
  bytes_down   bigint NOT NULL,
  raw_up       bigint NOT NULL DEFAULT 0,
  raw_down     bigint NOT NULL DEFAULT 0,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (day, account_id, node_id)
);
CREATE TRIGGER traffic_daily_touch BEFORE UPDATE ON traffic_daily FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ============ 业务模块 ============
CREATE TABLE announcements (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  title       jsonb NOT NULL,                 -- {"zh-CN": "...", "en": "..."}
  body_md     jsonb NOT NULL,
  audience    text NOT NULL DEFAULT 'all' CHECK (audience IN ('all','active','plans')),
  plan_ids    uuid[],
  pinned      boolean NOT NULL DEFAULT false,
  starts_at   timestamptz NOT NULL,
  ends_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER announcements_touch BEFORE UPDATE ON announcements FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE support_tickets (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  number      text NOT NULL UNIQUE,           -- TCK-YYYYMMDD-XXXXXX（CONV-02）
  account_id  uuid NOT NULL REFERENCES accounts(id),
  subject     text NOT NULL,
  category    text NOT NULL,
  status      text NOT NULL DEFAULT 'open' CHECK (status IN ('open','in_progress','waiting_user','closed')),
  assignee_id uuid REFERENCES accounts(id),
  status_changed_at timestamptz NOT NULL,     -- 自动关闭与重新打开的判定依据（OPS-11）
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX support_tickets_account ON support_tickets (account_id, created_at);
CREATE TRIGGER support_tickets_touch BEFORE UPDATE ON support_tickets FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE support_messages (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  ticket_id   uuid NOT NULL REFERENCES support_tickets(id) ON DELETE CASCADE,
  author_id   uuid NOT NULL REFERENCES accounts(id),
  body        text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX support_messages_ticket ON support_messages (ticket_id, created_at);
CREATE TRIGGER support_messages_touch BEFORE UPDATE ON support_messages FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 工单附件：先上传再在消息中引用（OPS-10）
CREATE TABLE support_attachments (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  uploader_id   uuid NOT NULL REFERENCES accounts(id),
  message_id    uuid REFERENCES support_messages(id) ON DELETE CASCADE,   -- 被消息引用前为空
  content_type  text NOT NULL CHECK (content_type IN ('image/png','image/jpeg','image/webp')),
  size_bytes    bigint NOT NULL CHECK (size_bytes > 0 AND size_bytes <= 5242880),
  sha256        bytea NOT NULL,
  storage_key   text NOT NULL UNIQUE,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX support_attachments_message ON support_attachments (message_id);
CREATE TRIGGER support_attachments_touch BEFORE UPDATE ON support_attachments FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE articles (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  slug        text NOT NULL UNIQUE,
  category    text NOT NULL,
  platforms   text[] NOT NULL DEFAULT '{}',
  title       jsonb NOT NULL,
  body_md     jsonb NOT NULL,
  published   boolean NOT NULL DEFAULT false,
  sort        int NOT NULL DEFAULT 0,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER articles_touch BEFORE UPDATE ON articles FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 通知模板：按模板、语言、渠道存放，只允许白名单变量（OPS-03）
CREATE TABLE notification_templates (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  template    text NOT NULL,
  locale      text NOT NULL,
  channel     text NOT NULL CHECK (channel IN ('email','telegram','webhook')),
  subject     text,
  body        text NOT NULL,
  version     int NOT NULL DEFAULT 1,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (template, locale, channel)
);
CREATE TRIGGER notification_templates_touch BEFORE UPDATE ON notification_templates FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 用户通知偏好：只有提醒类与营销类可以关闭（OPS-04、OPS-05）
CREATE TABLE notification_preferences (
  account_id  uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  category    text NOT NULL CHECK (category IN ('reminder','marketing')),
  channel     text NOT NULL CHECK (channel IN ('email','telegram')),
  is_enabled  boolean NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, category, channel)
);
CREATE TRIGGER notification_preferences_touch BEFORE UPDATE ON notification_preferences FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ============ 基础设施 ============
-- 业务事件 outbox（CONV-22、CONV-32）。载荷不含可识别个人的明文（CONV-29）
CREATE TABLE outbox (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),   -- 事件 ID
  topic           text NOT NULL,
  payload         jsonb NOT NULL,
  schema_version  int NOT NULL DEFAULT 1,
  published_at    timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX outbox_unpublished ON outbox (created_at) WHERE published_at IS NULL;
CREATE INDEX outbox_published ON outbox (published_at) WHERE published_at IS NOT NULL;
CREATE TRIGGER outbox_touch BEFORE UPDATE ON outbox FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 消费方幂等记录（CONV-32）
CREATE TABLE consumed_events (
  event_id    uuid NOT NULL,
  consumer    text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (event_id, consumer)
);

-- 外发通知队列（OPS-02）。含令牌、验证码或链接的变量加密存入 secret_variables_enc，投递结束后清除（CONV-31）
CREATE TABLE notification_outbox (
  id                    uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id            uuid REFERENCES accounts(id) ON DELETE CASCADE,
  channel               text NOT NULL CHECK (channel IN ('email','telegram','webhook')),
  template              text NOT NULL,
  locale                text NOT NULL,
  variables             jsonb NOT NULL,
  secret_variables_enc  bytea,
  attempts              int NOT NULL DEFAULT 0,
  next_attempt_at       timestamptz NOT NULL,
  retry_until           timestamptz NOT NULL,   -- 最长重试时间（OPS-02）
  sent_at               timestamptz,
  failed_at             timestamptz,
  last_error            text,
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notification_outbox_due ON notification_outbox (next_attempt_at) WHERE sent_at IS NULL AND failed_at IS NULL;
CREATE TRIGGER notification_outbox_touch BEFORE UPDATE ON notification_outbox FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 幂等键（CONV-12）：已认证请求按“账号 + 键”，未认证请求按“路由 + 键”唯一。
-- response_status 为空表示首个请求仍在处理中
CREATE TABLE idempotency_keys (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  key               uuid NOT NULL,
  account_id        uuid,
  route             text NOT NULL,
  request_hash      bytea NOT NULL,             -- 请求体 SHA-256
  response_status   int,
  response_headers  jsonb,
  response_body     bytea,
  expires_at        timestamptz NOT NULL,       -- 创建后 24 小时
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idempotency_keys_account_uq ON idempotency_keys (account_id, key) WHERE account_id IS NOT NULL;
CREATE UNIQUE INDEX idempotency_keys_route_uq ON idempotency_keys (route, key) WHERE account_id IS NULL;
CREATE INDEX idempotency_keys_expiry ON idempotency_keys (expires_at);
CREATE TRIGGER idempotency_keys_touch BEFORE UPDATE ON idempotency_keys FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 周期任务的 fencing token（spec/40 DEP-08）
CREATE TABLE job_fencing (
  job         text PRIMARY KEY,
  token       bigint NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER job_fencing_touch BEFORE UPDATE ON job_fencing FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- 审计日志，只追加（AUTH-18）
CREATE TABLE audit_logs (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  actor_id     uuid,
  action       text NOT NULL,
  target_type  text NOT NULL,
  target_id    text,
  diff         jsonb,                          -- _enc 与 _hash 字段只记录“已修改”
  ip_prefix    text,
  request_id   text,
  reason_id    uuid REFERENCES reason_texts(id),   -- 原因文本（AUTH-18、CONV-29）
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_target ON audit_logs (target_type, target_id, created_at);
CREATE INDEX audit_logs_actor ON audit_logs (actor_id, created_at);
CREATE TRIGGER audit_logs_append_only BEFORE UPDATE OR DELETE ON audit_logs FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER audit_logs_no_truncate BEFORE TRUNCATE ON audit_logs FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();

-- 内置角色（AUTH-17 权限目录）
INSERT INTO roles (name, permissions, is_builtin) VALUES
  ('superadmin', ARRAY['*'], true),
  ('operator',   ARRAY['accounts.read','accounts.adjust','orders.read','plans.*','location-groups.*','hosts.*',
                       'kernels.write','coupons.*','content.*','settings.read'], true),
  ('support',    ARRAY['accounts.read','orders.read','tickets.*'], true);
