-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 设置项（spec/03 settings，键名见 spec/03 3.6 与管理接口）。

-- name: GetSetting :one
SELECT value FROM settings WHERE key = sqlc.arg(key);

-- name: InitSetting :exec
-- 写入尚不存在的设置项；已存在时不变（并发时先写入者生效）。
INSERT INTO settings (key, value) VALUES (sqlc.arg(key), sqlc.arg(value)) ON CONFLICT (key) DO NOTHING;
