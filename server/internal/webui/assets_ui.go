// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !noui

package webui

import (
	"embed"
	"io/fs"
)

// dist 由 make build 填充：dist/portal、dist/admin 与 dist/build.json（spec/40 DEP-01）。
//
//go:embed all:dist
var dist embed.FS

// Embedded 返回嵌入的前端产物根目录（包含 portal/、admin/ 与 build.json）。
// 以 -tags noui 构建时返回 nil（DEP-06）。
func Embedded() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
