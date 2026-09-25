-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 管理员邀请（spec/10 AUTH-22）。状态由列推导：accepted、revoked、expired（按注入的时钟，CONV-27）、pending。

-- name: InsertInvitation :one
INSERT INTO staff_invitations (email, token_hash, inviter_id, expires_at)
VALUES (sqlc.arg(email), sqlc.arg(token_hash), sqlc.arg(inviter_id), sqlc.arg(expires_at))
RETURNING id, created_at;

-- name: InsertInvitationRole :exec
INSERT INTO staff_invitation_roles (staff_invitation_id, role) VALUES (sqlc.arg(staff_invitation_id), sqlc.arg(role));

-- name: ListInvitations :many
-- 按 ID（UUIDv7）倒序分页（CONV-11）。
SELECT i.id, i.email, i.inviter_id, i.expires_at, i.accepted_at, i.revoked_at, i.created_at,
       COALESCE((SELECT array_agg(ir.role ORDER BY ir.role) FROM staff_invitation_roles ir
                 WHERE ir.staff_invitation_id = i.id), '{}')::text[] AS roles
FROM staff_invitations i
WHERE (sqlc.narg(status)::text IS NULL OR sqlc.narg(status)::text = CASE
         WHEN i.accepted_at IS NOT NULL THEN 'accepted'
         WHEN i.revoked_at IS NOT NULL THEN 'revoked'
         WHEN i.expires_at <= sqlc.arg(now) THEN 'expired'
         ELSE 'pending' END)
  AND (sqlc.narg(before)::uuid IS NULL OR i.id < sqlc.narg(before)::uuid)
ORDER BY i.id DESC
LIMIT sqlc.arg(max_rows);

-- name: GetInvitation :one
SELECT i.id, i.email, i.inviter_id, i.expires_at, i.accepted_at, i.revoked_at, i.created_at,
       COALESCE((SELECT array_agg(ir.role ORDER BY ir.role) FROM staff_invitation_roles ir
                 WHERE ir.staff_invitation_id = i.id), '{}')::text[] AS roles
FROM staff_invitations i
WHERE i.id = sqlc.arg(id);

-- name: LockInvitation :one
SELECT id, email, inviter_id, expires_at, accepted_at, revoked_at FROM staff_invitations WHERE id = sqlc.arg(id) FOR UPDATE;

-- name: LockInvitationByToken :one
-- 接受邀请时锁定邀请行，令牌只能使用一次（AUTH-22）。
SELECT id, email, inviter_id, expires_at, accepted_at, revoked_at FROM staff_invitations
WHERE token_hash = sqlc.arg(token_hash) FOR UPDATE;

-- name: InvitationRoles :many
SELECT role FROM staff_invitation_roles WHERE staff_invitation_id = sqlc.arg(staff_invitation_id) ORDER BY role;

-- name: PendingInvitationExists :one
SELECT EXISTS (
  SELECT 1 FROM staff_invitations
  WHERE lower(email) = lower(sqlc.arg(email)::text) AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > sqlc.arg(now)
);

-- name: StaffEmailExists :one
SELECT EXISTS (
  SELECT 1 FROM accounts a JOIN account_roles ar ON ar.account_id = a.id WHERE lower(a.email) = lower(sqlc.arg(email)::text)
);

-- name: RevokeInvitation :exec
UPDATE staff_invitations SET revoked_at = sqlc.arg(now) WHERE id = sqlc.arg(id);

-- name: RevokePendingInvitationsBy :many
-- 超级管理员失去 superadmin 时撤销其发出的 pending 邀请（AUTH-22）。
UPDATE staff_invitations SET revoked_at = sqlc.arg(now)
WHERE inviter_id = sqlc.arg(inviter_id) AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > sqlc.arg(now)
RETURNING id;

-- name: AcceptInvitation :exec
UPDATE staff_invitations SET accepted_at = sqlc.arg(now), account_id = sqlc.arg(account_id) WHERE id = sqlc.arg(id);

-- name: LockAccountByEmail :one
-- 接受邀请时锁定被邀请邮箱的账号（AUTH-22 按账号状态处理）。
SELECT id, status, email_verified_at, locale FROM accounts WHERE lower(email) = lower(sqlc.arg(email)::text) FOR UPDATE;
