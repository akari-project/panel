-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 00004 的配套索引与约束校验（spec/03 3.6、CONV-21）。表中已有数据，使用 CONCURRENTLY，不在事务中执行：
--   - notification_outbox.staff_invitation_id：删除邀请时级联删除消息；
--   - staff_invitations：按邮箱查找 pending 邀请（邀请时的 taken 检查，AUTH-22）；
--   - audit_logs：按时间倒序分页与按时间筛选（spec/31 listAuditLogs，CONV-11）。

-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS notification_outbox_staff_invitation
  ON notification_outbox (staff_invitation_id) WHERE staff_invitation_id IS NOT NULL;
ALTER TABLE notification_outbox VALIDATE CONSTRAINT notification_outbox_one_recipient;
CREATE INDEX CONCURRENTLY IF NOT EXISTS staff_invitations_pending_email
  ON staff_invitations (lower(email)) WHERE accepted_at IS NULL AND revoked_at IS NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS audit_logs_created ON audit_logs (created_at, id);
