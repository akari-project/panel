// SPDX-License-Identifier: AGPL-3.0-or-later

package mfa

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 附录 B 的 SHA-1 测试向量（8 位码的末 6 位）。
func TestRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want8 := range map[int64]string{
		59:          "94287082",
		1111111109:  "07081804",
		1111111111:  "14050471",
		1234567890:  "89005924",
		2000000000:  "69279037",
		20000000000: "65353130",
	} {
		if got := Code(secret, Step(time.Unix(unix, 0))); got != want8[2:] {
			t.Errorf("t=%d: %s, want %s", unix, got, want8[2:])
		}
	}
}

func TestMatchWindowAndReuse(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1234567890, 0)
	cur := Step(now)
	for _, d := range []int64{-1, 0, 1} {
		if s, ok := Match(secret, Code(secret, cur+d), now, nil); !ok || s != cur+d {
			t.Errorf("offset %d rejected", d)
		}
	}
	if _, ok := Match(secret, Code(secret, cur+2), now, nil); ok {
		t.Error("code two steps ahead accepted")
	}
	last := cur
	if _, ok := Match(secret, Code(secret, cur), now, &last); ok {
		t.Error("code reused within the same step")
	}
	if _, ok := Match(secret, Code(secret, cur+1), now, &last); !ok {
		t.Error("next step rejected after use")
	}
}

func TestURI(t *testing.T) {
	u := URI("Akari", "a@example.com", []byte("12345678901234567890"))
	if !strings.HasPrefix(u, "otpauth://totp/Akari:a@example.com?") || !strings.Contains(u, "secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ") ||
		!strings.Contains(u, "issuer=Akari") {
		t.Fatal(u)
	}
}
