-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Idempotency-Key（CONV-12）。response_status 为空表示首个请求仍在处理；request_hash 为请求体的 HMAC-SHA256。

-- name: ClaimIdempotencyKey :one
-- 抢占幂等键：插入成功返回新行 ID；已存在（包括并发插入）时返回空行。
INSERT INTO idempotency_keys (key, account_id, route, request_hash, expires_at)
VALUES (sqlc.arg(key), sqlc.narg(account_id), sqlc.arg(route), sqlc.arg(request_hash), sqlc.arg(expires_at))
ON CONFLICT DO NOTHING
RETURNING id;

-- name: GetIdempotencyKey :one
SELECT id, route, request_hash, response_status, response_headers, response_body, expires_at
FROM idempotency_keys
WHERE key = sqlc.arg(key)
  AND ((sqlc.narg(account_id)::uuid IS NOT NULL AND account_id = sqlc.narg(account_id)::uuid)
    OR (sqlc.narg(account_id)::uuid IS NULL AND account_id IS NULL AND route = sqlc.arg(route)));

-- name: CompleteIdempotencyKey :execrows
UPDATE idempotency_keys
SET response_status = sqlc.arg(response_status), response_headers = sqlc.arg(response_headers), response_body = sqlc.arg(response_body)
WHERE id = sqlc.arg(id) AND response_status IS NULL;

-- name: DeleteIdempotencyKey :exec
DELETE FROM idempotency_keys WHERE id = sqlc.arg(id);

-- name: DeleteExpiredIdempotencyKeys :execrows
-- worker 每小时清理过期记录（CONV-12）。
DELETE FROM idempotency_keys WHERE expires_at <= sqlc.arg(now);
