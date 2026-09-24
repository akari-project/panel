-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 账号、注册与验证码（spec/10 AUTH-01–04）。

-- name: AccountByEmail :one
SELECT id, email, password_hash, status, email_verified_at, locale
FROM accounts WHERE lower(email) = lower(sqlc.arg(email)::text);

-- name: AccountByID :one
SELECT id, email, password_hash, status, email_verified_at, locale
FROM accounts WHERE id = sqlc.arg(id);

-- name: RegisterAccount :one
INSERT INTO accounts (email, password_hash, referral_code, referrer_id, locale, timezone)
VALUES (sqlc.arg(email), sqlc.arg(password_hash), sqlc.arg(referral_code), sqlc.narg(referrer_id), sqlc.arg(locale), sqlc.narg(timezone))
RETURNING id;

-- name: AccountByReferralCode :one
-- 账号邀请码（AUTH-02）。只有正常状态的账号的邀请码有效。
SELECT id FROM accounts WHERE referral_code = sqlc.arg(code) AND status = 'active';

-- name: MarkEmailVerified :exec
UPDATE accounts SET email_verified_at = sqlc.arg(now) WHERE id = sqlc.arg(id) AND email_verified_at IS NULL;

-- name: SetPasswordHash :exec
UPDATE accounts SET password_hash = sqlc.arg(password_hash) WHERE id = sqlc.arg(id);

-- name: InvalidateVerificationCodes :exec
-- 发送新码或新链接后旧的作废（AUTH-03、AUTH-04）。
UPDATE verification_codes SET consumed_at = sqlc.arg(now)
WHERE account_id = sqlc.arg(account_id) AND purpose = sqlc.arg(purpose) AND consumed_at IS NULL;

-- name: InsertVerificationCode :exec
INSERT INTO verification_codes (account_id, purpose, code_hash, expires_at)
VALUES (sqlc.arg(account_id), sqlc.arg(purpose), sqlc.arg(code_hash), sqlc.arg(expires_at));

-- name: ActiveVerificationCode :one
-- 当前有效的验证码（至多一条）。按 UUIDv7 主键取最新，不读取审计列 created_at（CONV-27）。
SELECT id, code_hash, attempts, expires_at FROM verification_codes
WHERE account_id = sqlc.arg(account_id) AND purpose = sqlc.arg(purpose) AND consumed_at IS NULL
ORDER BY id DESC LIMIT 1
FOR UPDATE;

-- name: VerificationCodeByHash :one
-- 找回密码链接按令牌哈希查找（AUTH-04）。
SELECT id, account_id, expires_at, consumed_at FROM verification_codes
WHERE code_hash = sqlc.arg(code_hash) AND purpose = sqlc.arg(purpose)
FOR UPDATE;

-- name: IncrementCodeAttempts :one
UPDATE verification_codes SET attempts = attempts + 1 WHERE id = sqlc.arg(id) RETURNING attempts;

-- name: ConsumeVerificationCode :exec
UPDATE verification_codes SET consumed_at = sqlc.arg(now) WHERE id = sqlc.arg(id);
