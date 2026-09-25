// SPDX-License-Identifier: AGPL-3.0-or-later

package clientconfig

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
)

// 以下函数按 spec/03 3.6 的读取总则解码 settings 键：读取永不因值异常而失败；参数为 nil 表示缺键，
// 取默认值；值异常时按各键的规定处理，并在 invalid 中列出键名，由调用方记录日志，不记录值（CONV-24）。

// RegistrationControl 是有效的注册控制（spec/10 AUTH-02）。
type RegistrationControl struct {
	// Policy 为 open、invite_only 或 closed。
	Policy string
	// Allow、Deny 为邮箱域名白名单与黑名单；Policy 因值异常为 closed 时为空。
	Allow, Deny []string
}

// Registration 解码三个注册控制键（registration_policy、email_domain_allowlist、email_domain_denylist）。
// 缺键按 open 与空名单；任一键值异常时有效策略为 closed（安全相关的键按关闭方向，spec/03 3.6）：
// 策略不是 "open"、"invite_only"、"closed" 三个 JSON 字符串之一，或名单不是数组（含 null）、含非字符串元素。
func Registration(policy, allow, deny []byte) (rc RegistrationControl, invalid []string) {
	rc.Policy = "open"
	if policy != nil {
		var p string
		if err := json.Unmarshal(policy, &p); err != nil || (p != "open" && p != "invite_only" && p != "closed") {
			invalid = append(invalid, "registration_policy")
		} else {
			rc.Policy = p
		}
	}
	var ok bool
	if rc.Allow, ok = domainList(allow); !ok {
		invalid = append(invalid, "email_domain_allowlist")
	}
	if rc.Deny, ok = domainList(deny); !ok {
		invalid = append(invalid, "email_domain_denylist")
	}
	if invalid != nil {
		return RegistrationControl{Policy: "closed"}, invalid
	}
	return rc, nil
}

// domainList 解码邮箱域名名单：缺键为空名单；不是数组或含非字符串元素时 ok 为 false。
func domainList(raw []byte) (list []string, ok bool) {
	if raw == nil {
		return nil, true
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, false
	}
	list = make([]string, 0, len(items))
	for _, it := range items {
		var d string
		if len(it) == 0 || it[0] != '"' || json.Unmarshal(it, &d) != nil {
			return nil, false
		}
		list = append(list, d)
	}
	return list, true
}

// MinVersion 解码 settings 键 min_version（spec/03 3.6、spec/30 API-03）。可用性优先（不是安全控制）：
// 值不是对象（含 null）时视为 {}，invalid 为 "min_version"；某平台的值不是合法 x.y.z 时忽略该平台，
// invalid 为 "min_version.<平台>"；未知平台键忽略，不算异常。
func MinVersion(raw []byte) (versions map[string]string, invalid []string) {
	versions = map[string]string{}
	if raw == nil {
		return versions, nil
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(raw, &stored); err != nil || stored == nil {
		return versions, []string{"min_version"}
	}
	for _, p := range Platforms {
		v, ok := stored[p]
		if !ok {
			continue
		}
		var s string
		if len(v) == 0 || v[0] != '"' || json.Unmarshal(v, &s) != nil || !ValidVersion(s) {
			invalid = append(invalid, "min_version."+p)
			continue
		}
		versions[p] = s
	}
	return versions, invalid
}

// SettingsWarner 按 settings 键分别记住最近一次告警的原始值摘要，使同一份异常值每个进程只告警一次；
// 不保存原文（CONV-24）。键只来自固定的 settings 键名，不会无界增长。零值可用。
type SettingsWarner struct {
	mu   sync.Mutex
	last map[string][sha256.Size]byte
}

// Changed 报告是否需要为 key 记录告警：bad 为 false（值合法或缺键）时清除该键的记录并返回 false；
// 否则只有原始值与上次告警的不同时返回 true。
func (w *SettingsWarner) Changed(key string, raw []byte, bad bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !bad {
		delete(w.last, key)
		return false
	}
	sum := sha256.Sum256(raw)
	if prev, ok := w.last[key]; ok && prev == sum {
		return false
	}
	if w.last == nil {
		w.last = map[string][sha256.Size]byte{}
	}
	w.last[key] = sum
	return true
}
