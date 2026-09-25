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
-- 同一事务中调用。
-- 已存的值必须是 JSON 字符串 YYYY-MM-DDTHH:MM:SSZ，否则转换失败、修改随之失败（fail-closed，只有直接改库才会出现）：
-- timestamptz 的输入还接受 'now'、'epoch'、'infinity' 与不带时区的字符串，JSON null 会使 GREATEST 忽略上一次的值，
-- 因此先按格式校验。ELSE 分支拼接行内的值（长度为 0），避免常量表达式在计划阶段被提前求值而无条件报错。
INSERT INTO settings (key, value)
VALUES ('config_issued_at', to_jsonb(to_char(date_trunc('second', sqlc.arg(now)::timestamptz) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')))
ON CONFLICT (key) DO UPDATE SET value = to_jsonb(to_char(
  GREATEST(
    date_trunc('second', sqlc.arg(now)::timestamptz),
    CASE
      WHEN jsonb_typeof(settings.value) = 'string' AND (settings.value #>> '{}') ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$'
        THEN (settings.value #>> '{}')::timestamptz
      ELSE ('invalid config_issued_at' || left(settings.value::text, 0))::timestamptz
    END + interval '1 second'
  ) AT TIME ZONE 'UTC',
  'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
RETURNING (value #>> '{}')::text AS issued_at;

-- name: GetSettings :many
-- 在一条语句中读取多个设置项，结果来自同一个快照（如三个注册控制键，spec/10 AUTH-02）。不存在的键不返回。
SELECT key, value FROM settings WHERE key = ANY(sqlc.arg(keys)::text[]);

-- name: ReplaceSettingIf :execrows
-- 仅当设置项仍为 old 时替换为 value（按 jsonb 相等比较），用于纠正异常值而不覆盖并发写入的新值。
UPDATE settings SET value = sqlc.arg(value) WHERE key = sqlc.arg(key) AND value = sqlc.arg(old)::jsonb;
