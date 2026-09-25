-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 角色（spec/10 AUTH-17、AUTH-22）。内置角色不可修改、不可删除，由应用层保证。

-- name: ListRoles :many
SELECT r.name, r.description, r.permissions, r.is_builtin,
       (SELECT count(*) FROM account_roles ar WHERE ar.role = r.name)::int AS staff_count,
       r.created_at, r.updated_at
FROM roles r
ORDER BY r.is_builtin DESC, r.name;

-- name: GetRole :one
SELECT r.name, r.description, r.permissions, r.is_builtin,
       (SELECT count(*) FROM account_roles ar WHERE ar.role = r.name)::int AS staff_count,
       r.created_at, r.updated_at
FROM roles r
WHERE r.name = sqlc.arg(name);

-- name: LockRole :one
SELECT name, description, permissions, is_builtin, updated_at FROM roles WHERE name = sqlc.arg(name) FOR UPDATE;

-- name: CountRoles :one
SELECT count(*) FROM roles;

-- name: CountExistingRoles :one
SELECT count(*) FROM roles WHERE name = ANY(sqlc.arg(names)::text[]);

-- name: InsertRole :exec
INSERT INTO roles (name, description, permissions) VALUES (sqlc.arg(name), sqlc.narg(description), sqlc.arg(permissions));

-- name: UpdateRole :exec
UPDATE roles SET description = sqlc.narg(description), permissions = sqlc.arg(permissions)
WHERE name = sqlc.arg(name) AND NOT is_builtin;

-- name: DeleteRole :exec
DELETE FROM roles WHERE name = sqlc.arg(name) AND NOT is_builtin;

-- name: RoleHasPendingInvitation :one
-- 仍被 pending 邀请引用的角色不能删除（AUTH-22）。
SELECT EXISTS (
  SELECT 1 FROM staff_invitation_roles ir JOIN staff_invitations i ON i.id = ir.staff_invitation_id
  WHERE ir.role = sqlc.arg(role) AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > sqlc.arg(now)
);

-- name: ClearLegacyInvitationRole :exec
-- staff_invitations.role 已废弃（spec/03 3.6），但旧行仍以外键引用角色，删除角色前清空。
UPDATE staff_invitations SET role = NULL WHERE role = sqlc.arg(role);
