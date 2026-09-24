-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 当前账号信息（spec/30 GET /v1/me）。

-- name: GetMe :one
-- entitlement_status：当前权益（active、over_quota、suspended，至多一条，BIL-05）属于免费套餐时为 free，
-- 否则为其状态；没有当前权益时为 none（spec/11 11.1）。
-- timezone：账号未设置时取站点时区 site_timezone，默认 Asia/Shanghai（CONV-26）。
-- device_limit：取当前权益快照；没有当前权益时取设置项 free_device_limit，默认 1（spec/10 AUTH-14）。
SELECT a.id, a.email, a.email_verified_at, a.status, a.locale,
       COALESCE(a.timezone, (SELECT s.value #>> '{}' FROM settings s WHERE s.key = 'site_timezone'), 'Asia/Shanghai')::text AS timezone, a.referral_code, a.auto_renew, a.created_at,
       t.enabled_at AS totp_enabled_at,
       e.status AS entitlement_status, p.kind AS plan_kind, e.device_limit AS entitlement_device_limit,
       COALESCE((SELECT (s.value #>> '{}')::int FROM settings s WHERE s.key = 'free_device_limit'), 1)::int AS free_device_limit
FROM accounts a
LEFT JOIN mfa_totp t ON t.account_id = a.id
LEFT JOIN entitlements e ON e.account_id = a.id AND e.status IN ('active', 'over_quota', 'suspended')
LEFT JOIN plans p ON p.id = e.plan_id
WHERE a.id = sqlc.arg(id);
