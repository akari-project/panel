// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build noui

package webui

import "io/fs"

// Embedded 在 noui 构建中返回 nil：二进制不含前端（spec/40 DEP-06）。
func Embedded() fs.FS { return nil }
