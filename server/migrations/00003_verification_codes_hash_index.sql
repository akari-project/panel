-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 找回密码链接按令牌哈希查找（spec/10 AUTH-04），为 verification_codes.code_hash 建索引。
-- 表中已有数据，按 CONV-21 使用 CONCURRENTLY，不在事务中执行。

-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS verification_codes_hash ON verification_codes (code_hash);
