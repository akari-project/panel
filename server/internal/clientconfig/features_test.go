// SPDX-License-Identifier: AGPL-3.0-or-later

package clientconfig

import (
	"encoding/json"
	"slices"
	"testing"
)

// spec/03 3.6、OPS-08：只有 JSON true 且已实现才算开启；非对象、非布尔值视为关闭并只报告键名；未知键忽略。
func TestFeatures(t *testing.T) {
	none := map[string]bool{}
	support := map[string]bool{"support": true}
	for _, tc := range []struct {
		name        string
		raw         string // 空串表示键不存在
		implemented map[string]bool
		on          []string
		invalid     []string
	}{
		{"missing", "", support, nil, nil},
		{"empty object", `{}`, support, nil, nil},
		{"null", `null`, support, nil, []string{"features"}},
		{"array", `[true]`, support, nil, []string{"features"}},
		{"string", `"support"`, support, nil, []string{"features"}},
		{"number", `1`, support, nil, []string{"features"}},
		{"true implemented", `{"support":true}`, support, []string{"support"}, nil},
		{"true not implemented", `{"support":true,"articles":true}`, none, nil, nil},
		{"false", `{"support":false}`, support, nil, nil},
		{"string true", `{"support":"true"}`, support, nil, []string{"features.support"}},
		{"number one", `{"support":1}`, support, nil, []string{"features.support"}},
		{"null value", `{"support":null}`, support, nil, []string{"features.support"}},
		{"object value", `{"support":{},"referrals":[]}`, support, nil, []string{"features.support", "features.referrals"}},
		{"unknown keys", `{"support":true,"unknown":"x","other":true}`, support, []string{"support"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			if tc.raw != "" {
				raw = []byte(tc.raw)
			}
			got, invalid := Features(raw, tc.implemented)
			if len(got) != len(Modules) {
				t.Fatalf("modules = %v", got)
			}
			for _, m := range Modules {
				if got[m] != slices.Contains(tc.on, m) {
					t.Errorf("%s = %v", m, got[m])
				}
			}
			if !slices.Equal(invalid, tc.invalid) {
				t.Errorf("invalid = %q, want %q", invalid, tc.invalid)
			}
		})
	}
}

// M1 阶段五个模块都未实现：已存 true 的有效值恒为 false。在 Implemented 中登记模块时同步修改本测试。
func TestFeaturesNoneImplemented(t *testing.T) {
	all := map[string]bool{}
	for _, m := range Modules {
		all[m] = true
	}
	raw, _ := json.Marshal(all)
	got, invalid := Features(raw, Implemented)
	for m, v := range got {
		if v {
			t.Errorf("%s enabled though not implemented", m)
		}
	}
	if invalid != nil {
		t.Errorf("invalid = %q", invalid)
	}
}
