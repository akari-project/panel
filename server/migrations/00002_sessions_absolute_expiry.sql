-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 会话链的绝对失效时间（spec/03 3.6，spec/10 AUTH-07、AUTH-21）：客户端会话自首次登录起 90 天，管理会话 12 小时。
-- 首次登录时由应用用注入的时钟写入（CONV-27），轮换时由子会话继承。
-- 列可空以兼容上一版本二进制（spec/40 DEP-12）：旧二进制插入的行为空，此时以 expires_at 为准。
-- 只加可空列、没有默认值，不重写表，可以在线执行。

-- +goose Up
ALTER TABLE sessions ADD COLUMN absolute_expires_at timestamptz;
COMMENT ON COLUMN sessions.absolute_expires_at IS '会话链的绝对失效时间，轮换时继承；为空时以 expires_at 为准（AUTH-07）';
