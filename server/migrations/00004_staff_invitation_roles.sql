-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 管理员、角色与邀请（spec/03 3.6，spec/10 AUTH-22）：
--   - roles.description：自定义角色的描述；
--   - staff_invitation_roles：一次邀请授予的角色，可以多个；取代 staff_invitations.role。
--     staff_invitations.role 按 spec/40 DEP-12 分两步废弃：本迁移去掉 NOT NULL，应用不再读写，下一个小版本删除；
--   - notification_outbox.staff_invitation_id：邀请邮件的收件人（收件人可能还没有账号），
--     投递时从 staff_invitations.email 读取地址，outbox 不保存邮箱（CONV-29）。索引见 00005。
-- 只加可空列、新表与 CHECK，不重写表，可以在线执行（DEP-12）。

-- +goose Up
ALTER TABLE roles ADD COLUMN description text;

-- 纯关联表，没有 updated_at（CONV-17）。role 级联删除：已接受、已撤销、已过期的邀请不阻止删除角色；
-- 仍被 pending 邀请引用的角色由应用层拒绝删除（409 invalid_state）。
CREATE TABLE staff_invitation_roles (
  staff_invitation_id  uuid NOT NULL REFERENCES staff_invitations(id) ON DELETE CASCADE,
  role                 text NOT NULL REFERENCES roles(name) ON DELETE CASCADE,
  created_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (staff_invitation_id, role)
);
CREATE INDEX staff_invitation_roles_role ON staff_invitation_roles (role);
INSERT INTO staff_invitation_roles (staff_invitation_id, role)
  SELECT id, role FROM staff_invitations WHERE role IS NOT NULL;

ALTER TABLE staff_invitations ALTER COLUMN role DROP NOT NULL;
COMMENT ON COLUMN staff_invitations.role IS '废弃：由 staff_invitation_roles 取代，应用不再读写（spec/03 3.6）';

ALTER TABLE notification_outbox
  ADD COLUMN staff_invitation_id uuid REFERENCES staff_invitations(id) ON DELETE CASCADE;
-- NOT VALID：不在本事务中扫描已有行；00005 在事务外执行 VALIDATE（新列在已有行中都为空，必然通过）。
ALTER TABLE notification_outbox
  ADD CONSTRAINT notification_outbox_one_recipient CHECK (num_nonnulls(account_id, staff_invitation_id) <= 1) NOT VALID;
