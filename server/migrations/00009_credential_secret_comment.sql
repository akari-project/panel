-- SPDX-License-Identifier: AGPL-3.0-or-later
-- proxy_credentials.secret_enc 的明文格式（spec/21 AGT-15、spec/23 EXP-09）。00001 已提交，不可修改（CONV-21），
-- 在这里以注释注明。早期 `panel admin create` 写入的 36 字符文本在读取时解析（account.OpenCredentialSecret）。

-- +goose Up
COMMENT ON COLUMN proxy_credentials.secret_enc IS
  'AEAD ciphertext (CONV-19, CONV-30) whose plaintext is the raw 16-byte UUIDv4 credential secret (spec/21 AGT-15); '
  'protocol forms are derived per spec/23 EXP-09. Legacy rows may hold the 36-character text form, parsed on read.';
