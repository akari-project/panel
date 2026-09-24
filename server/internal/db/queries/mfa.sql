-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 二次验证（spec/10 AUTH-11、AUTH-12、AUTH-20、AUTH-23）。

-- name: GetTotp :one
SELECT secret_enc, recovery_hashes, last_used_step, enabled_at FROM mfa_totp
WHERE account_id = sqlc.arg(account_id)
FOR UPDATE;

-- name: InsertTotp :exec
INSERT INTO mfa_totp (account_id, secret_enc, recovery_hashes, last_used_step, enabled_at)
VALUES (sqlc.arg(account_id), sqlc.arg(secret_enc), sqlc.arg(recovery_hashes), sqlc.narg(last_used_step), sqlc.arg(enabled_at));

-- name: SetTotpStep :exec
UPDATE mfa_totp SET last_used_step = sqlc.arg(step) WHERE account_id = sqlc.arg(account_id);

-- name: SetRecoveryHashes :exec
UPDATE mfa_totp SET recovery_hashes = sqlc.arg(recovery_hashes) WHERE account_id = sqlc.arg(account_id);

-- name: DeleteTotp :exec
DELETE FROM mfa_totp WHERE account_id = sqlc.arg(account_id);

-- name: HasStaffRole :one
-- 持有任一管理员角色（AUTH-12：管理员必须启用二次验证）。
SELECT EXISTS (SELECT 1 FROM account_roles WHERE account_id = sqlc.arg(account_id));

-- name: RevokeConsoleSessions :many
-- 管理员的二次验证设置变化时吊销其全部管理会话（AUTH-21）。
UPDATE sessions SET revoked_at = sqlc.arg(now)
WHERE account_id = sqlc.arg(account_id) AND audience = 'console' AND revoked_at IS NULL
RETURNING id;

-- name: RevokeOtherSessions :many
-- 修改密码后吊销除当前会话外的全部会话（spec/30 PUT /v1/me/password）。
UPDATE sessions SET revoked_at = sqlc.arg(now)
WHERE account_id = sqlc.arg(account_id) AND revoked_at IS NULL AND id <> sqlc.arg(keep)
RETURNING id;
