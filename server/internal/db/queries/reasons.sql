-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 原因文本（CONV-29）。

-- name: ClearAccountReasonTexts :execrows
-- 删除账号个人数据时清空其原因文本（spec/10 AUTH-05），行保留以维持只追加表的引用。
UPDATE reason_texts SET body = '' WHERE account_id = sqlc.arg(account_id)::uuid AND body <> '';
