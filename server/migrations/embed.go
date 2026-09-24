// SPDX-License-Identifier: AGPL-3.0-or-later

// Package migrations 把 goose SQL 迁移嵌入二进制，由 `panel migrate` 执行（spec/40 DEP-12）。
// 迁移只前进，已提交的文件不可修改（CONV-21）。
package migrations

import "embed"

// FS 包含本目录下全部 NNNNN_描述.sql 迁移。
//
//go:embed *.sql
var FS embed.FS
