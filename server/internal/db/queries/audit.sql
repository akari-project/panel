-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 审计日志（spec/10 AUTH-18，spec/31 CON-09）。audit_logs 只追加（CONV-18），原因原文在 reason_texts（CONV-29）。

-- name: InsertReasonText :one
INSERT INTO reason_texts (account_id, body) VALUES (sqlc.narg(account_id), sqlc.arg(body))
RETURNING id;

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (actor_id, action, target_type, target_id, diff, ip_prefix, request_id, reason_id)
VALUES (sqlc.narg(actor_id), sqlc.arg(action), sqlc.arg(target_type), sqlc.narg(target_id), sqlc.narg(diff),
        sqlc.narg(ip_prefix), sqlc.narg(request_id), sqlc.narg(reason_id));

-- name: ListAuditLogs :many
-- 按 (created_at, id) 倒序的游标分页（CONV-11）；created_at 只用于展示与筛选，不参与业务判断（CONV-27）。
SELECT l.id, l.actor_id, a.email AS actor_email, l.action, l.target_type, l.target_id, l.diff,
       r.body AS reason, l.ip_prefix, l.request_id, l.created_at
FROM audit_logs l
LEFT JOIN accounts a ON a.id = l.actor_id
LEFT JOIN reason_texts r ON r.id = l.reason_id
WHERE (sqlc.narg(actor_id)::uuid IS NULL OR l.actor_id = sqlc.narg(actor_id)::uuid)
  AND (sqlc.narg(action)::text IS NULL OR l.action = sqlc.narg(action)::text)
  AND (sqlc.narg(target_type)::text IS NULL OR l.target_type = sqlc.narg(target_type)::text)
  AND (sqlc.narg(target_id)::text IS NULL OR l.target_id = sqlc.narg(target_id)::text)
  AND (sqlc.narg(created_from)::timestamptz IS NULL OR l.created_at >= sqlc.narg(created_from)::timestamptz)
  AND (sqlc.narg(created_to)::timestamptz IS NULL OR l.created_at <= sqlc.narg(created_to)::timestamptz)
  AND (sqlc.narg(cursor_at)::timestamptz IS NULL
       OR (l.created_at, l.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY l.created_at DESC, l.id DESC
LIMIT sqlc.arg(max_rows);

-- name: GetAuditLog :one
SELECT l.id, l.actor_id, a.email AS actor_email, l.action, l.target_type, l.target_id, l.diff,
       r.body AS reason, l.ip_prefix, l.request_id, l.created_at
FROM audit_logs l
LEFT JOIN accounts a ON a.id = l.actor_id
LEFT JOIN reason_texts r ON r.id = l.reason_id
WHERE l.id = sqlc.arg(id);
