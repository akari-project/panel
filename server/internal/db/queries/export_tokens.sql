-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 第三方客户端的导出令牌（spec/10 AUTH-16，CONV-20 例外：token_hash 用于查找，token_enc 用于再次显示）。

-- name: InsertExportToken :exec
-- 账号创建时生成；读取导入链接时若不存在（AUTH-22 的凭据重置会删除）同样补建。已有令牌时不变。
INSERT INTO export_tokens (account_id, token_hash, token_enc, rotated_at)
VALUES (sqlc.arg(account_id), sqlc.arg(token_hash), sqlc.arg(token_enc), sqlc.arg(now))
ON CONFLICT (account_id) DO NOTHING;

-- name: BackfillExportToken :exec
-- 读取导入链接时补建缺失的令牌：只为正常或暂停的账号补建，正在注销与已注销的账号不补建（AUTH-05）。
INSERT INTO export_tokens (account_id, token_hash, token_enc, rotated_at)
SELECT a.id, sqlc.arg(token_hash), sqlc.arg(token_enc), sqlc.arg(now)
FROM accounts a WHERE a.id = sqlc.arg(account_id) AND a.status IN ('active', 'suspended')
ON CONFLICT (account_id) DO NOTHING;

-- name: ExportToken :one
SELECT token_enc, rotated_at FROM export_tokens WHERE account_id = sqlc.arg(account_id);

-- name: ReplaceExportToken :exec
-- 重置导出令牌（AUTH-16）：旧令牌的哈希随之删除，旧链接立即失效。
INSERT INTO export_tokens (account_id, token_hash, token_enc, rotated_at)
VALUES (sqlc.arg(account_id), sqlc.arg(token_hash), sqlc.arg(token_enc), sqlc.arg(now))
ON CONFLICT (account_id) DO UPDATE SET token_hash = EXCLUDED.token_hash, token_enc = EXCLUDED.token_enc, rotated_at = EXCLUDED.rotated_at;

-- name: RevokeSharedCredential :many
-- 吊销账号的共用凭据（device_id 为空），返回凭据 ID 以写 credential.changed。
UPDATE proxy_credentials SET revoked_at = sqlc.arg(now)
WHERE account_id = sqlc.arg(account_id) AND device_id IS NULL AND revoked_at IS NULL
RETURNING id;
