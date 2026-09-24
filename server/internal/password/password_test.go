// SPDX-License-Identifier: AGPL-3.0-or-later

package password

import (
	"strings"
	"testing"
)

// 测试用低成本参数；默认参数另测编码。
var fast = Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1}

func TestHashVerify(t *testing.T) {
	h, err := Hash("correct horse", fast)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("encoding = %s", h)
	}
	if ok, err := Verify("correct horse", h); !ok || err != nil {
		t.Fatalf("Verify correct = %v, %v", ok, err)
	}
	if ok, _ := Verify("wrong horse", h); ok {
		t.Fatal("Verify accepted wrong password")
	}
	if _, err := Verify("x", "$bcrypt$..."); err == nil {
		t.Fatal("Verify accepted unknown format")
	}
}

func TestDefaultParamsEncoding(t *testing.T) {
	if DefaultParams != (Params{MemoryKiB: 65536, Iterations: 3, Parallelism: 1}) {
		t.Fatalf("DefaultParams = %+v, want AUTH-01 values", DefaultParams)
	}
}

func TestCheckLength(t *testing.T) {
	for pw, ok := range map[string]bool{
		"1234567":                false,
		"12345678":               true,
		strings.Repeat("密", 128): true,
		strings.Repeat("a", 129): false,
	} {
		if err := CheckLength(pw); (err == nil) != ok {
			t.Errorf("CheckLength(len %d) = %v", len(pw), err)
		}
	}
}
