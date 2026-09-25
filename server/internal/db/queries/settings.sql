-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 设置项（spec/03 settings，键名见 spec/03 3.6 与管理接口）。

-- name: GetSetting :one
SELECT value FROM settings WHERE key = sqlc.arg(key);

-- name: InitSetting :exec
-- 写入尚不存在的设置项；已存在时不变（并发时先写入者生效）。
INSERT INTO settings (key, value) VALUES (sqlc.arg(key), sqlc.arg(value)) ON CONFLICT (key) DO NOTHING;

-- name: BumpConfigIssuedAt :one
-- 更新 config_issued_at（spec/30 API-11）：写入 max(当前时刻, 上一次的值 + 1 秒)，秒精度 RFC 3339 UTC，严格递增；
-- 键缺失时写入当前时刻。单条语句，ON CONFLICT DO UPDATE 锁住该行直到事务结束，并发修改不会得到相同的值。
-- now 由调用方从注入的时钟取得（CONV-04、CONV-27）；在修改 features、registration_policy、min_version 有效值的
-- 同一事务中调用。已存的值不是合法时间时报错，修改随之失败（只有直接改库才会出现）。
INSERT INTO settings (key, value)
VALUES ('config_issued_at', to_jsonb(to_char(date_trunc('second', sqlc.arg(now)::timestamptz) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')))
ON CONFLICT (key) DO UPDATE SET value = to_jsonb(to_char(
  GREATEST(date_trunc('second', sqlc.arg(now)::timestamptz), (settings.value #>> '{}')::timestamptz + interval '1 second') AT TIME ZONE 'UTC',
  'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
RETURNING (value #>> '{}')::text AS issued_at;
