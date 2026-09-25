// SPDX-License-Identifier: AGPL-3.0-or-later

package clientconfig

import (
	"slices"
	"testing"
)

func rawOrNil(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}

// spec/03 3.6、AUTH-02：缺键按 open 与空名单；任一注册控制键值异常时策略为 closed，只报告键名。
func TestRegistration(t *testing.T) {
	for _, tc := range []struct {
		name                string
		policy, allow, deny string // 空串表示缺键
		want                RegistrationControl
		invalid             []string
	}{
		{"missing", "", "", "", RegistrationControl{Policy: "open"}, nil},
		{"open", `"open"`, `[]`, `[]`, RegistrationControl{Policy: "open", Allow: []string{}, Deny: []string{}}, nil},
		{"invite_only", `"invite_only"`, "", "", RegistrationControl{Policy: "invite_only"}, nil},
		{"closed", `"closed"`, "", "", RegistrationControl{Policy: "closed"}, nil},
		{"lists", "", `["corp.example"]`, `["spam.example",""]`,
			RegistrationControl{Policy: "open", Allow: []string{"corp.example"}, Deny: []string{"spam.example", ""}}, nil},
		{"unknown policy", `"members_only"`, "", "", RegistrationControl{Policy: "closed"}, []string{"registration_policy"}},
		{"empty policy", `""`, "", "", RegistrationControl{Policy: "closed"}, []string{"registration_policy"}},
		{"uppercase policy", `"OPEN"`, "", "", RegistrationControl{Policy: "closed"}, []string{"registration_policy"}},
		{"number policy", `1`, "", "", RegistrationControl{Policy: "closed"}, []string{"registration_policy"}},
		{"null policy", `null`, "", "", RegistrationControl{Policy: "closed"}, []string{"registration_policy"}},
		{"object policy", `{}`, "", "", RegistrationControl{Policy: "closed"}, []string{"registration_policy"}},
		{"allow string", `"open"`, `"corp.example"`, "", RegistrationControl{Policy: "closed"}, []string{"email_domain_allowlist"}},
		{"allow null", `"open"`, `null`, "", RegistrationControl{Policy: "closed"}, []string{"email_domain_allowlist"}},
		{"allow object", `"open"`, `{}`, "", RegistrationControl{Policy: "closed"}, []string{"email_domain_allowlist"}},
		{"deny number element", `"open"`, "", `[1]`, RegistrationControl{Policy: "closed"}, []string{"email_domain_denylist"}},
		{"deny null element", `"open"`, "", `["a.example",null]`, RegistrationControl{Policy: "closed"}, []string{"email_domain_denylist"}},
		{"all invalid", `true`, `1`, `[[]]`, RegistrationControl{Policy: "closed"},
			[]string{"registration_policy", "email_domain_allowlist", "email_domain_denylist"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, invalid := Registration(rawOrNil(tc.policy), rawOrNil(tc.allow), rawOrNil(tc.deny))
			if got.Policy != tc.want.Policy || !slices.Equal(got.Allow, tc.want.Allow) || !slices.Equal(got.Deny, tc.want.Deny) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			if !slices.Equal(invalid, tc.invalid) {
				t.Errorf("invalid = %q, want %q", invalid, tc.invalid)
			}
		})
	}
}

// spec/03 3.6、API-03：min_version 可用性优先，非对象视为 {}，非法版本忽略并报告，未知平台忽略不报告。
func TestMinVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		want    map[string]string
		invalid []string
	}{
		{"missing", "", map[string]string{}, nil},
		{"empty", `{}`, map[string]string{}, nil},
		{"valid", `{"ios":"1.4.0","linux":"0.10.2"}`, map[string]string{"ios": "1.4.0", "linux": "0.10.2"}, nil},
		{"array", `["1.0.0"]`, map[string]string{}, []string{"min_version"}},
		{"string", `"1.0.0"`, map[string]string{}, []string{"min_version"}},
		{"null", `null`, map[string]string{}, []string{"min_version"}},
		{"number", `1`, map[string]string{}, []string{"min_version"}},
		{"bad versions", `{"ios":"01.0.0","android":1,"macos":null,"windows":"1.0.0"}`,
			map[string]string{"windows": "1.0.0"}, []string{"min_version.ios", "min_version.android", "min_version.macos"}},
		{"unknown platform", `{"web":"x","ios":"1.0.0"}`, map[string]string{"ios": "1.0.0"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, invalid := MinVersion(rawOrNil(tc.raw))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for p, v := range tc.want {
				if got[p] != v {
					t.Errorf("%s = %q, want %q", p, got[p], v)
				}
			}
			if !slices.Equal(invalid, tc.invalid) {
				t.Errorf("invalid = %q, want %q", invalid, tc.invalid)
			}
		})
	}
}

// SettingsWarner：同一份异常值只告警一次；值合法后清除；各键互不影响。
func TestSettingsWarner(t *testing.T) {
	var w SettingsWarner
	a, b := []byte(`1`), []byte(`2`)
	steps := []struct {
		key  string
		raw  []byte
		bad  bool
		want bool
	}{
		{"min_version", a, true, true},
		{"min_version", a, true, false},
		{"features", a, true, true}, // 另一个键
		{"min_version", a, true, false},
		{"min_version", b, true, true}, // 另一份异常值
		{"min_version", nil, false, false},
		{"min_version", b, true, true}, // 恢复合法后再次出现
		{"features", a, true, false},
	}
	for i, s := range steps {
		if got := w.Changed(s.key, s.raw, s.bad); got != s.want {
			t.Errorf("step %d (%s %s %v) = %v", i, s.key, s.raw, s.bad, got)
		}
	}
}
