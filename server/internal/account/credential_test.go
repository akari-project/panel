// SPDX-License-Identifier: AGPL-3.0-or-later

package account

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/secretbox"
)

func TestRandomCode(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		c := randomCode(8)
		if len(c) != 8 || strings.ContainsAny(c, "01ILO") {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 990 {
		t.Errorf("only %d distinct codes out of 1000", len(seen))
	}
}

// AGT-15：凭据 secret 以 16 字节原始 UUIDv4 加密保存；早期写入的 36 字符文本在读取时解析为同一值，其他形式拒绝。
func TestOpenCredentialSecret(t *testing.T) {
	keys, err := secretbox.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	want := uuid.New()
	seal := func(pt []byte, ad []byte) []byte {
		ct, err := keys.Seal(pt, ad)
		if err != nil {
			t.Fatal(err)
		}
		return ct
	}
	for name, pt := range map[string][]byte{"raw": want[:], "legacy text": []byte(want.String())} {
		got, err := OpenCredentialSecret(keys, seal(pt, CredentialSecretAD))
		if err != nil || got != want {
			t.Errorf("%s: got %s %v, want %s", name, got, err, want)
		}
	}
	for name, pt := range map[string][]byte{
		"empty":       {},
		"15 bytes":    want[:15],
		"32 hex":      []byte(strings.ReplaceAll(want.String(), "-", "")),
		"36 non-uuid": []byte(strings.Repeat("z", 36)),
		"braced":      []byte("{" + want.String() + "}"),
	} {
		if _, err := OpenCredentialSecret(keys, seal(pt, CredentialSecretAD)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// 附加数据绑定列：其他列的密文不能当作凭据打开。
	if _, err := OpenCredentialSecret(keys, seal(want[:], []byte("export_tokens.token_enc"))); err == nil {
		t.Error("ciphertext sealed for another column accepted")
	}
}
