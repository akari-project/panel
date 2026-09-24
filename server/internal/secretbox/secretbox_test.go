// SPDX-License-Identifier: AGPL-3.0-or-later

package secretbox

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func key(id byte, fill byte) string {
	return string(rune('0'+id)) + ":" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, KeySize))
}

func TestRoundTripAndFormat(t *testing.T) {
	k, err := ParseKeyring(key(1, 0xaa), "")
	if err != nil {
		t.Fatal(err)
	}
	ct, err := k.Seal([]byte("secret"), []byte("proxy_credentials.secret_enc"))
	if err != nil {
		t.Fatal(err)
	}
	if ct[0] != 1 || len(ct) != 1+24+len("secret")+16 {
		t.Fatalf("ciphertext layout: key_id=%d len=%d", ct[0], len(ct))
	}
	pt, err := k.Open(ct, []byte("proxy_credentials.secret_enc"))
	if err != nil || string(pt) != "secret" {
		t.Fatalf("Open = %q, %v", pt, err)
	}
	if _, err := k.Open(ct, []byte("other.column")); !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrong AD: %v", err)
	}
	ct[len(ct)-1] ^= 1
	if _, err := k.Open(ct, []byte("proxy_credentials.secret_enc")); !errors.Is(err, ErrDecrypt) {
		t.Errorf("tampered: %v", err)
	}
}

func TestRotation(t *testing.T) {
	old, _ := ParseKeyring(key(1, 0x01), "")
	ct, _ := old.Seal([]byte("v"), nil)
	rotated, err := ParseKeyring(key(2, 0x02), key(1, 0x01))
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := rotated.Open(ct, nil); err != nil || string(pt) != "v" {
		t.Fatalf("old ciphertext with previous key: %q %v", pt, err)
	}
	if !rotated.NeedsRotation(ct) {
		t.Error("NeedsRotation = false for old key")
	}
	fresh, _ := rotated.Seal([]byte("v"), nil)
	if fresh[0] != 2 || rotated.NeedsRotation(fresh) {
		t.Error("new ciphertext should use current key")
	}
	onlyNew, _ := ParseKeyring(key(2, 0x02), "")
	if _, err := onlyNew.Open(ct, nil); !errors.Is(err, ErrDecrypt) {
		t.Errorf("after previous key retired: %v", err)
	}
}

func TestParseKeyErrors(t *testing.T) {
	for _, s := range []string{"", "abc", "0:" + strings.Repeat("A", 44), "1:short", "300:" + strings.Repeat("A", 44)} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%q) accepted", s)
		}
	}
	if _, err := ParseKeyring(key(1, 1), key(1, 2)); err == nil {
		t.Error("same key_id for current and previous accepted")
	}
}
