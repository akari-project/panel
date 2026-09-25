// SPDX-License-Identifier: AGPL-3.0-or-later

package clientconfig

import (
	"bytes"
	"encoding/json"
)

// Modules 为 settings 键 features 的模块名（spec/13 OPS-08）。
var Modules = []string{"announcements", "articles", "support", "referrals", "diagnostics"}

// Implemented 为本二进制已实现的模块（OPS-08）。实现某个模块时在此登记；M1 阶段均未实现。
var Implemented = map[string]bool{}

// Features 按 spec/03 3.6 解码 settings 键 features 并返回各模块的有效值（OPS-08）：
// 存储值为 JSON true 且 implemented 中已登记才为 true。raw 为 nil 表示键不存在，全部关闭。
//
// 解码永不失败：值不是对象时全部关闭，某模块的值不是布尔时该模块关闭，并在 invalid 中列出键名
// （"features" 或 "features.<模块名>"），由调用方记录日志，不记录值（CONV-24）。未知键忽略；
// 未实现模块残留的 true 是有意保留的（OPS-08），不算异常。
func Features(raw []byte, implemented map[string]bool) (effective map[string]bool, invalid []string) {
	effective = make(map[string]bool, len(Modules))
	for _, m := range Modules {
		effective[m] = false
	}
	if raw == nil {
		return effective, nil
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(raw, &stored); err != nil || stored == nil {
		return effective, []string{"features"}
	}
	for _, m := range Modules {
		v, ok := stored[m]
		if !ok {
			continue
		}
		switch string(bytes.TrimSpace(v)) {
		case "true":
			effective[m] = implemented[m]
		case "false":
		default:
			invalid = append(invalid, "features."+m)
		}
	}
	return effective, invalid
}
