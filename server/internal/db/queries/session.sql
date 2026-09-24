-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 登录、设备与会话（spec/10 AUTH-06–10、AUTH-13、AUTH-14）。

-- name: LoginAccount :one
SELECT a.id, a.password_hash, a.status, (t.enabled_at IS NOT NULL)::bool AS totp_enabled
FROM accounts a LEFT JOIN mfa_totp t ON t.account_id = a.id
WHERE lower(a.email) = lower(sqlc.arg(email)::text);

-- name: InsertDevice :one
INSERT INTO devices (account_id, platform, model, app_version, public_key, last_seen_at)
VALUES (sqlc.arg(account_id), sqlc.arg(platform), sqlc.narg(model), sqlc.narg(app_version), sqlc.narg(public_key), sqlc.arg(now))
RETURNING id;

-- name: DeviceForReuse :one
SELECT id, platform, public_key FROM devices
WHERE id = sqlc.arg(id) AND account_id = sqlc.arg(account_id) AND revoked_at IS NULL
FOR UPDATE;

-- name: TouchDevice :exec
UPDATE devices SET model = sqlc.narg(model), app_version = sqlc.narg(app_version), last_seen_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: ActiveWebDevicesOverLimit :many
-- 每个账号最多保留 50 个未吊销的 web 设备，超出时吊销最早活跃的（AUTH-10）。
SELECT id FROM devices
WHERE account_id = sqlc.arg(account_id) AND platform = 'web' AND revoked_at IS NULL
ORDER BY last_seen_at ASC NULLS FIRST, id ASC
OFFSET sqlc.arg(keep)::int;

-- name: CountActiveDevices :one
-- 设备上限只统计未吊销的非 web 设备（AUTH-14）。
SELECT count(*) FROM devices
WHERE account_id = sqlc.arg(account_id) AND platform <> 'web' AND revoked_at IS NULL AND id <> sqlc.arg(exclude);

-- name: RevokeDevice :exec
UPDATE devices SET revoked_at = sqlc.arg(now) WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: DeviceCredential :one
SELECT id FROM proxy_credentials WHERE device_id = sqlc.arg(device_id) AND revoked_at IS NULL;

-- name: CreateDeviceCredential :one
INSERT INTO proxy_credentials (account_id, device_id, secret_enc) VALUES (sqlc.arg(account_id), sqlc.arg(device_id), sqlc.arg(secret_enc))
RETURNING id;

-- name: RevokeDeviceCredentials :many
UPDATE proxy_credentials SET revoked_at = sqlc.arg(now)
WHERE device_id = sqlc.arg(device_id) AND revoked_at IS NULL
RETURNING id;

-- name: CredentialEntitlement :one
-- 下发凭据所依据的当前权益：只有 active 可以下发（over_quota、suspended 不下发，spec/30 CredentialStatus）。
SELECT e.status, e.device_limit FROM entitlements e
WHERE e.account_id = sqlc.arg(account_id) AND e.status IN ('active', 'over_quota', 'suspended');

-- name: InsertSession :exec
INSERT INTO sessions (id, account_id, device_id, audience, refresh_token_hash, parent_id, user_agent, ip_prefix, expires_at, absolute_expires_at)
VALUES (sqlc.arg(id), sqlc.arg(account_id), sqlc.narg(device_id), sqlc.arg(audience), sqlc.arg(refresh_token_hash), sqlc.narg(parent_id),
        sqlc.narg(user_agent), sqlc.narg(ip_prefix), sqlc.arg(expires_at), sqlc.narg(absolute_expires_at));

-- name: SessionByRefreshHash :one
SELECT s.id, s.account_id, s.device_id, s.audience, s.parent_id, s.user_agent, s.ip_prefix, s.expires_at, s.absolute_expires_at,
       s.used_at, s.revoked_at, a.status AS account_status, d.revoked_at AS device_revoked_at
FROM sessions s
JOIN accounts a ON a.id = s.account_id
LEFT JOIN devices d ON d.id = s.device_id
WHERE s.refresh_token_hash = sqlc.arg(refresh_token_hash)
FOR UPDATE OF s;

-- name: ChildSession :one
SELECT id, user_agent, ip_prefix FROM sessions WHERE parent_id = sqlc.arg(parent_id);

-- name: MarkSessionUsed :exec
UPDATE sessions SET used_at = sqlc.arg(now) WHERE id = sqlc.arg(id);

-- name: RevokeSessionChain :many
-- 吊销会话所在的整条轮换链（AUTH-07）：先沿 parent_id 找到根，再取根的全部后代。
WITH RECURSIVE up AS (
  SELECT id, parent_id FROM sessions WHERE sessions.id = sqlc.arg(id)
  UNION
  SELECT s.id, s.parent_id FROM sessions s JOIN up ON s.id = up.parent_id
), root AS (
  SELECT id FROM up WHERE parent_id IS NULL
), down AS (
  SELECT id FROM root
  UNION
  SELECT s.id FROM sessions s JOIN down ON s.parent_id = down.id
)
UPDATE sessions SET revoked_at = sqlc.arg(now)
WHERE sessions.id IN (SELECT id FROM down) AND revoked_at IS NULL
RETURNING sessions.id;

-- name: RevokeDeviceSessions :many
UPDATE sessions SET revoked_at = sqlc.arg(now)
WHERE device_id = sqlc.arg(device_id) AND revoked_at IS NULL
RETURNING id;

-- name: RevokeAccountSessions :many
UPDATE sessions SET revoked_at = sqlc.arg(now)
WHERE account_id = sqlc.arg(account_id) AND revoked_at IS NULL
RETURNING id;

-- name: SessionDevice :one
SELECT device_id FROM sessions WHERE id = sqlc.arg(id) AND account_id = sqlc.arg(account_id);

-- name: LockAccount :one
-- 串行化同一账号的设备凭据下发，使设备上限的计数与插入之间不会并发超额（AUTH-14）。
SELECT id FROM accounts WHERE id = sqlc.arg(id) FOR UPDATE;

-- name: ActiveDeviceKeyExists :one
-- 同一账号未吊销的设备公钥不得重复（AUTH-10）。
SELECT EXISTS (
  SELECT 1 FROM devices
  WHERE account_id = sqlc.arg(account_id) AND public_key = sqlc.arg(public_key) AND revoked_at IS NULL
);
