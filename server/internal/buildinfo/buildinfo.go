// SPDX-License-Identifier: AGPL-3.0-or-later

// Package buildinfo 保存构建时通过 -ldflags 写入的版本与 git 提交（spec/40 DEP-01）：
//
//	-X github.com/akari-project/panel/server/internal/buildinfo.Version=v0.1.0
//	-X github.com/akari-project/panel/server/internal/buildinfo.Commit=<git 提交>
package buildinfo

// 由 -ldflags 写入；未写入时为默认值。
var (
	Version = "dev"
	Commit  = ""
)
