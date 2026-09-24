// SPDX-License-Identifier: AGPL-3.0-or-later

package clientconfig

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

func testSigner(t *testing.T, id string, b byte) *Signer {
	t.Helper()
	s, err := ParseKey(id + ":" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseKey(t *testing.T) {
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	for _, bad := range []string{"", "7", "0:" + seed, "256:" + seed, "x:" + seed, "1:" + base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
	if s := testSigner(t, "12", 7); s.KeyID() != "12" {
		t.Errorf("key id = %q", s.KeyID())
	}
}

// RFC 8785 3.2.3 的排序示例：按 UTF-16 码元排序，补充平面字符（代理对）排在 U+FB33 之前。
func TestCanonicalOrderAndEscaping(t *testing.T) {
	got, err := Canonical(map[string]any{
		"\u20ac": "Euro Sign", "\r": "Carriage Return", "\ufb33": "Hebrew Letter Dalet With Dagesh",
		"1": "One", "\U0001f600": "Emoji: Grinning Face", "\u0080": "Control", "\u00f6": "Latin Small Letter O With Diaeresis",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"\u00f6\":\"Latin Small Letter O With Diaeresis\"," +
		"\"\u20ac\":\"Euro Sign\",\"\U0001f600\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}"
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	got, _ = Canonical(map[string]any{"s": "<a&b>\"\\\x01\u2028", "n": int64(-3), "t": true, "l": []string{"x"}, "z": map[string]any{}, "e": []any{}})
	if want := `{"e":[],"l":["x"],"n":-3,"s":"<a&b>\"\\\u0001` + "\u2028" + `","t":true,"z":{}}`; string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if _, err := Canonical(map[string]any{"f": 1.5}); err == nil {
		t.Fatal("non-integer accepted")
	}
	if got, _ := Canonical(map[string]any{"n": float64(17)}); string(got) != `{"n":17}` {
		t.Fatalf("integral float64 = %s", got)
	}
}

// 签名确定：同一 payload 得到相同的签名与 ETag；payload 变化时 ETag 变化；签名可用公钥验证。
func TestSignDeterministic(t *testing.T) {
	s := testSigner(t, "1", 3)
	p := map[string]any{"min_version": map[string]any{"ios": "1.2.0"}, "announcement_version": int64(0)}
	a, err := s.Sign(p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Sign(map[string]any{"announcement_version": int64(0), "min_version": map[string]any{"ios": "1.2.0"}})
	if a.ETag != b.ETag || !bytes.Equal(a.Signature, b.Signature) {
		t.Fatal("signature not deterministic")
	}
	if !ed25519.Verify(s.PublicKey(), a.Payload, a.Signature) || len(a.Signature) != 64 {
		t.Fatal("signature does not verify")
	}
	c, _ := s.Sign(map[string]any{"announcement_version": int64(1), "min_version": map[string]any{"ios": "1.2.0"}})
	if c.ETag == a.ETag {
		t.Fatal("etag unchanged after payload change")
	}
}

func TestUserAgent(t *testing.T) {
	if _, err := NewUserAgent("Akari App"); err == nil {
		t.Fatal("non-token app name accepted")
	}
	u, err := NewUserAgent("Akari")
	if err != nil {
		t.Fatal(err)
	}
	min := map[string]string{"ios": "1.4.0", "android": "2.0.0"}
	for _, tc := range []struct {
		ua       string
		ok       bool
		platform string
		outdated bool
	}{
		{"Akari/1.3.9 (iOS 19.1)", true, "ios", true},
		{"Akari/1.4.0 (iOS 19.1)", true, "ios", false},
		{"Akari/1.10.0 (iPadOS 19.1)", true, "ios", false},
		{"Akari/1.4.0-beta.1+42 (IOS; arm64)", true, "ios", false},
		{"Akari/1.9.9 (Android 16)", true, "android", true},
		{"Akari/0.1.0 (Windows NT 10.0)", true, "windows", false}, // 未配置的平台不比较
		{"Akari/0.1.0 (Haiku R1)", true, "", false},               // 无法识别的平台不比较
		{"Akari/0.1.0", true, "", false},
		{"Akari/1.3.0 okhttp/5.0 (Android 16) extra (iOS)", true, "android", true}, // 第一个括号
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 19_1 like Mac OS X)", false, "", false},
		{"AkariX/0.1.0 (iOS 19.1)", false, "", false},
		{"clash-verge/1.0.0 (iOS 19.1)", false, "", false},
	} {
		c, ok := u.Parse(tc.ua)
		if ok != tc.ok || c.Platform != tc.platform || (ok && Outdated(c, min) != tc.outdated) {
			t.Errorf("%q: ok=%v platform=%q outdated=%v", tc.ua, ok, c.Platform, ok && Outdated(c, min))
		}
	}
}

func TestValidVersion(t *testing.T) {
	for v, want := range map[string]bool{"1.2.0": true, "0.0.0": true, "10.20.30": true, "01.2.0": false, "1.2": false, "1.2.0-beta": false, "": false} {
		if ValidVersion(v) != want {
			t.Errorf("ValidVersion(%q) = %v", v, !want)
		}
	}
	if strings.Contains(strings.Join(Platforms, ","), "web") {
		t.Error("web is not a client platform")
	}
}
