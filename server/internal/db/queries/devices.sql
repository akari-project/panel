-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 设备列表、移除设备与设备凭据的分配（spec/10 AUTH-13–15，spec/30 /v1/me/devices）。

-- name: CredentialSlots :many
-- 未吊销的非 web 设备及其未吊销的凭据，按 last_seen_at 从近到远排序（AUTH-14）。调用方已持有账号行锁。
SELECT d.id, c.id AS credential_id
FROM devices d
LEFT JOIN proxy_credentials c ON c.device_id = d.id AND c.revoked_at IS NULL
WHERE d.account_id = sqlc.arg(account_id) AND d.platform <> 'web' AND d.revoked_at IS NULL
ORDER BY d.last_seen_at DESC NULLS LAST, d.id
FOR NO KEY UPDATE OF d;

-- name: RevokeCredential :exec
UPDATE proxy_credentials SET revoked_at = sqlc.arg(now) WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: TouchDeviceSeen :exec
-- 刷新令牌时记录设备最近活跃时间（AUTH-14 的排序依据）。设备行正被移除或分配凭据时跳过，
-- 不在持有会话行锁时等待设备行锁（移除设备先锁设备、再吊销会话）。
UPDATE devices SET last_seen_at = sqlc.arg(now)
WHERE id = (SELECT id FROM devices WHERE devices.id = sqlc.arg(id) AND revoked_at IS NULL FOR NO KEY UPDATE SKIP LOCKED);

-- name: ListDevices :many
-- 未吊销的设备（含 web 设备）。ip_prefix 取该设备最近的未吊销会话；is_current 为发起请求的会话所属的设备。
SELECT d.id, d.platform, d.model, d.app_version, d.created_at, d.last_seen_at,
       (SELECT s.ip_prefix FROM sessions s
        WHERE s.account_id = d.account_id AND s.device_id = d.id AND s.revoked_at IS NULL
        ORDER BY s.id DESC LIMIT 1) AS ip_prefix,
       (d.id IS NOT DISTINCT FROM (SELECT s.device_id FROM sessions s WHERE s.id = sqlc.arg(session_id) AND s.account_id = d.account_id))::bool AS is_current,
       EXISTS (SELECT 1 FROM proxy_credentials c WHERE c.device_id = d.id AND c.revoked_at IS NULL)::bool AS has_credential
FROM devices d
WHERE d.account_id = sqlc.arg(account_id) AND d.revoked_at IS NULL
ORDER BY d.last_seen_at DESC NULLS LAST, d.id;

-- name: DeviceLimit :one
-- 显示用的设备上限，与 GET /v1/me 的 device_limit 口径相同：当前权益快照，没有当前权益时取 free_device_limit（默认 1）。
SELECT COALESCE(
  (SELECT e.device_limit FROM entitlements e WHERE e.account_id = sqlc.arg(account_id) AND e.status IN ('active', 'over_quota', 'suspended')),
  (SELECT (s.value #>> '{}')::int FROM settings s WHERE s.key = 'free_device_limit'),
  1)::int AS device_limit;

-- name: LockOwnDevice :one
-- 移除设备（AUTH-15）：只锁定本账号未吊销的设备；他人的设备与不存在的设备同样找不到（CONV-15）。
-- 设备行一律取 FOR NO KEY UPDATE：刷新令牌插入子会话时对设备行取 FOR KEY SHARE（外键），两者不冲突。
SELECT id FROM devices WHERE id = sqlc.arg(id) AND account_id = sqlc.arg(account_id) AND revoked_at IS NULL FOR NO KEY UPDATE;
