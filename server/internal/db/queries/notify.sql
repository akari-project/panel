-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 外发通知队列（spec/13 OPS-02，CONV-31）。

-- name: EnqueueNotification :exec
INSERT INTO notification_outbox (account_id, channel, template, locale, variables, secret_variables_enc, next_attempt_at, retry_until)
VALUES (sqlc.narg(account_id), sqlc.arg(channel), sqlc.arg(template), sqlc.arg(locale), sqlc.arg(variables),
        sqlc.narg(secret_variables_enc), sqlc.arg(next_attempt_at), sqlc.arg(retry_until));

-- name: ClaimDueNotifications :many
-- 取出到期的消息并锁定；多个 worker 并行时各取不同的行。收件地址在投递时从账号读取（CONV-29）。
SELECT n.id, n.channel, n.template, n.locale, n.variables, n.secret_variables_enc, n.attempts, n.retry_until,
       a.email
FROM notification_outbox n
JOIN accounts a ON a.id = n.account_id
WHERE n.sent_at IS NULL AND n.failed_at IS NULL AND n.next_attempt_at <= sqlc.arg(now)
ORDER BY n.next_attempt_at
LIMIT sqlc.arg(max_rows)
FOR UPDATE OF n SKIP LOCKED;

-- name: MarkNotificationSent :exec
UPDATE notification_outbox
SET sent_at = sqlc.arg(now), attempts = attempts + 1, secret_variables_enc = NULL, last_error = NULL
WHERE id = sqlc.arg(id);

-- name: MarkNotificationRetry :exec
UPDATE notification_outbox
SET attempts = attempts + 1, next_attempt_at = sqlc.arg(next_attempt_at), last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id);

-- name: MarkNotificationFailed :exec
-- 最终失败：清除秘密变量（CONV-31）。
UPDATE notification_outbox
SET failed_at = sqlc.arg(now), attempts = attempts + 1, secret_variables_enc = NULL, last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id);

-- name: GetNotificationTemplate :one
SELECT subject, body FROM notification_templates
WHERE template = sqlc.arg(template) AND locale = sqlc.arg(locale) AND channel = sqlc.arg(channel);
