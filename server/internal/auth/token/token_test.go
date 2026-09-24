// SPDX-License-Identifier: AGPL-3.0-or-later

package token

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/clock"
)

func envKey(id string, b byte) string {
	return id + ":" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

var t0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

func TestIssueVerify(t *testing.T) {
	clk := clock.NewFake(t0)
	k, err := NewKeyring(clk, envKey("1", 1), "")
	if err != nil {
		t.Fatal(err)
	}
	in := Claims{AccountID: uuid.New(), SessionID: uuid.New(), Audience: AudienceConsole, AMR: []string{"pwd", "otp"}}
	tok, issued, err := k.Issue(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "v4.public.") {
		t.Fatalf("token %q is not v4.public", tok)
	}
	if !issued.ExpiresAt.Equal(t0.Add(TTL)) {
		t.Fatalf("expires_at = %v", issued.ExpiresAt)
	}
	got, err := k.Verify(tok, AudienceConsole)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != in.AccountID || got.SessionID != in.SessionID || got.Audience != AudienceConsole ||
		strings.Join(got.AMR, ",") != "pwd,otp" || !got.ExpiresAt.Equal(issued.ExpiresAt) {
		t.Fatalf("claims = %+v", got)
	}
}

func TestVerifyRejects(t *testing.T) {
	clk := clock.NewFake(t0)
	k, _ := NewKeyring(clk, envKey("1", 1), "")
	other, _ := NewKeyring(clk, envKey("1", 2), "")
	tok, _, _ := k.Issue(Claims{AccountID: uuid.New(), SessionID: uuid.New(), Audience: AudienceClient})
	forged, _, _ := other.Issue(Claims{AccountID: uuid.New(), SessionID: uuid.New(), Audience: AudienceClient})

	cases := map[string]func() error{
		"wrong audience":     func() error { _, err := k.Verify(tok, AudienceConsole); return err },
		"other key same kid": func() error { _, err := k.Verify(forged, AudienceClient); return err },
		"tampered": func() error {
			b := []byte(tok)
			b[len("v4.public.")+3] ^= 1
			_, err := k.Verify(string(b), AudienceClient)
			return err
		},
		"garbage":  func() error { _, err := k.Verify("v4.public.x", AudienceClient); return err },
		"v4.local": func() error { _, err := k.Verify("v4.local.AAAA", AudienceClient); return err },
		"expired": func() error {
			clk.Set(t0.Add(TTL))
			defer clk.Set(t0)
			_, err := k.Verify(tok, AudienceClient)
			return err
		},
	}
	for name, f := range cases {
		if err := f(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := k.Verify(tok, AudienceClient); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
}

// 轮换：新密钥签发，旧密钥签发的令牌在旧密钥下线前仍可验证，下线后失效（AUTH-06）。
func TestRotation(t *testing.T) {
	clk := clock.NewFake(t0)
	oldK, _ := NewKeyring(clk, envKey("1", 1), "")
	oldTok, _, _ := oldK.Issue(Claims{AccountID: uuid.New(), SessionID: uuid.New(), Audience: AudienceClient})

	rotated, err := NewKeyring(clk, envKey("2", 2), envKey("1", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.Verify(oldTok, AudienceClient); err != nil {
		t.Fatalf("old token rejected during rotation: %v", err)
	}
	newTok, _, _ := rotated.Issue(Claims{AccountID: uuid.New(), SessionID: uuid.New(), Audience: AudienceClient})
	if _, err := oldK.Verify(newTok, AudienceClient); !errors.Is(err, ErrInvalid) {
		t.Fatalf("new token verified with old keyring only: %v", err)
	}
	retired, _ := NewKeyring(clk, envKey("2", 2), "")
	if _, err := retired.Verify(oldTok, AudienceClient); !errors.Is(err, ErrInvalid) {
		t.Fatalf("old token accepted after retirement: %v", err)
	}
}

func TestKeyringErrors(t *testing.T) {
	clk := clock.NewFake(t0)
	for name, c := range map[string][2]string{
		"missing":      {"", ""},
		"no colon":     {"abc", ""},
		"bad id":       {envKey("0", 1), ""},
		"short":        {"1:" + base64.StdEncoding.EncodeToString([]byte("short")), ""},
		"same id":      {envKey("1", 1), envKey("1", 2)},
		"bad previous": {envKey("1", 1), "x"},
		"id too large": {envKey("256", 1), ""},
	} {
		if _, err := NewKeyring(clk, c[0], c[1]); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
