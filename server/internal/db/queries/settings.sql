-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 设置项（spec/03 settings，键名见 spec/03 3.6 与管理接口）。

-- name: GetSetting :one
SELECT value FROM settings WHERE key = sqlc.arg(key);
