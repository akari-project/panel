-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 管理员（spec/10 AUTH-12、AUTH-21、AUTH-22）。管理员即持有至少一个角色的账号。

-- name: StaffPrincipal :one
-- 管理接口每个请求读取一次（AUTH-12、AUTH-17）：账号状态、是否启用 TOTP、角色与展开后的权限。
-- 角色与二次验证的变化因此立即生效，不依赖访问令牌过期。
SELECT a.status, a.email,
       EXISTS (SELECT 1 FROM mfa_totp t WHERE t.account_id = a.id)::boolean AS has_totp,
       EXISTS (SELECT 1 FROM mfa_webauthn w WHERE w.account_id = a.id)::boolean AS has_passkey,
       COALESCE((SELECT array_agg(ar.role ORDER BY ar.role) FROM account_roles ar WHERE ar.account_id = a.id), '{}')::text[] AS roles,
       COALESCE((SELECT array_agg(DISTINCT p ORDER BY p)
                 FROM account_roles ar JOIN roles r ON r.name = ar.role, unnest(r.permissions) AS p
                 WHERE ar.account_id = a.id), '{}')::text[] AS permissions
FROM accounts a
WHERE a.id = sqlc.arg(id);

-- name: ListStaff :many
-- 按账号 ID 分页（CONV-11）。最近登录时间见 StaffLastLogins。
SELECT a.id, a.email,
       array_agg(ar.role ORDER BY ar.role)::text[] AS roles,
       EXISTS (SELECT 1 FROM mfa_totp t WHERE t.account_id = a.id)::boolean AS has_totp,
       EXISTS (SELECT 1 FROM mfa_webauthn w WHERE w.account_id = a.id)::boolean AS has_passkey,
       min(ar.created_at)::timestamptz AS created_at
FROM accounts a
JOIN account_roles ar ON ar.account_id = a.id
WHERE (sqlc.narg(role)::text IS NULL
       OR EXISTS (SELECT 1 FROM account_roles x WHERE x.account_id = a.id AND x.role = sqlc.narg(role)::text))
  AND (sqlc.narg(after)::uuid IS NULL OR a.id > sqlc.narg(after)::uuid)
GROUP BY a.id
ORDER BY a.id
LIMIT sqlc.arg(max_rows);

-- name: GetStaff :one
SELECT a.id, a.email,
       array_agg(ar.role ORDER BY ar.role)::text[] AS roles,
       EXISTS (SELECT 1 FROM mfa_totp t WHERE t.account_id = a.id)::boolean AS has_totp,
       EXISTS (SELECT 1 FROM mfa_webauthn w WHERE w.account_id = a.id)::boolean AS has_passkey,
       min(ar.created_at)::timestamptz AS created_at
FROM accounts a
JOIN account_roles ar ON ar.account_id = a.id
WHERE a.id = sqlc.arg(id)
GROUP BY a.id;

-- name: AccountRoles :many
SELECT role FROM account_roles WHERE account_id = sqlc.arg(account_id) ORDER BY role;

-- name: DeleteAccountRoles :exec
DELETE FROM account_roles WHERE account_id = sqlc.arg(account_id);

-- name: RoleHolders :many
SELECT account_id FROM account_roles WHERE role = sqlc.arg(role);

-- name: DeleteWebauthn :exec
DELETE FROM mfa_webauthn WHERE account_id = sqlc.arg(account_id);

-- name: StaffLastLogins :many
-- 最近登录时间：管理会话链的根会话的建立时间，只用于展示（CONV-27）。从未登录的账号没有行。
SELECT account_id, max(created_at)::timestamptz AS last_login_at
FROM sessions
WHERE account_id = ANY(sqlc.arg(account_ids)::uuid[]) AND audience = 'console' AND parent_id IS NULL
GROUP BY account_id;
