-- SPDX-License-Identifier: AGPL-3.0-or-later
-- `panel admin create` 使用的查询（spec/10 AUTH-21）。

-- name: AccountEmailExists :one
SELECT EXISTS (SELECT 1 FROM accounts WHERE lower(email) = lower(sqlc.arg(email)::text));

-- name: CountRoleMembers :one
SELECT count(*) FROM account_roles WHERE role = sqlc.arg(role);

-- name: CreateAccount :one
INSERT INTO accounts (email, password_hash, email_verified_at, referral_code)
VALUES (sqlc.arg(email), sqlc.arg(password_hash), sqlc.arg(email_verified_at), sqlc.arg(referral_code))
RETURNING id;

-- name: ReferralCodeExists :one
SELECT EXISTS (SELECT 1 FROM accounts WHERE referral_code = sqlc.arg(referral_code));

-- name: GrantRole :exec
INSERT INTO account_roles (account_id, role) VALUES (sqlc.arg(account_id), sqlc.arg(role));

-- name: CreateSharedCredential :one
INSERT INTO proxy_credentials (account_id, secret_enc) VALUES (sqlc.arg(account_id), sqlc.arg(secret_enc))
RETURNING id;

-- name: InsertOutboxEvent :one
INSERT INTO outbox (topic, payload, schema_version) VALUES (sqlc.arg(topic), sqlc.arg(payload), sqlc.arg(schema_version))
RETURNING id;
