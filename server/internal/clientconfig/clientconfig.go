// SPDX-License-Identifier: AGPL-3.0-or-later

// Package clientconfig 生成已签名的客户端启动配置 GET /v1/config（spec/30 API-11），
// 并按 User-Agent 判断自研客户端是否低于最低版本（API-03）。
package clientconfig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Signer 持有 /v1/config 的 Ed25519 签名私钥（CONV-30 PANEL_CONFIG_KEY）。
type Signer struct {
	keyID string
	sk    ed25519.PrivateKey
}

// ParseKey 解析 "<key_id>:<base64 的 32 字节种子>"，key_id 为 1–255，格式与 PANEL_TOKEN_KEY 相同（CONV-30）。
func ParseKey(s string) (*Signer, error) {
	if s == "" {
		return nil, errors.New("clientconfig: PANEL_CONFIG_KEY is not set")
	}
	id, b64, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return nil, errors.New("clientconfig: key must be <key_id>:<base64>")
	}
	n, err := strconv.Atoi(id)
	if err != nil || n < 1 || n > 255 {
		return nil, errors.New("clientconfig: key_id must be 1–255")
	}
	seed, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("clientconfig: key must be %d bytes of standard base64", ed25519.SeedSize)
	}
	return &Signer{keyID: strconv.Itoa(n), sk: ed25519.NewKeyFromSeed(seed)}, nil
}

// KeyID 返回十进制字符串形式的 key id。
func (s *Signer) KeyID() string { return s.keyID }

// PublicKey 返回验签公钥。
func (s *Signer) PublicKey() ed25519.PublicKey { return s.sk.Public().(ed25519.PublicKey) }

// Document 是签名后的启动配置。
type Document struct {
	// Payload 为 payload 的 RFC 8785 规范化字节，即签名对象。
	Payload   []byte
	Signature []byte
	KeyID     string
	// Bytes 为整个签名文档 {payload, signature, key_id} 的规范化字节，即响应体。
	Bytes []byte
	// ETag 为强 ETag：Bytes 的 SHA-256（CONV-13）。
	ETag string
}

// Sign 规范化 payload 并签名。Ed25519 签名是确定性的（RFC 8032），相同 payload 在各副本得到相同的文档与 ETag。
func (s *Signer) Sign(payload map[string]any) (Document, error) {
	canon, err := Canonical(payload)
	if err != nil {
		return Document{}, err
	}
	sig := ed25519.Sign(s.sk, canon)
	doc, err := Canonical(map[string]any{
		"payload":   RawJSON(canon),
		"signature": base64.StdEncoding.EncodeToString(sig),
		"key_id":    s.keyID,
	})
	if err != nil {
		return Document{}, err
	}
	sum := sha256.Sum256(doc)
	return Document{Payload: canon, Signature: sig, KeyID: s.keyID, Bytes: doc, ETag: `"` + hex.EncodeToString(sum[:]) + `"`}, nil
}

// RawJSON 是已经规范化的 JSON 片段，Canonical 原样写出。
type RawJSON []byte

// Canonical 按 RFC 8785（JCS）序列化 payload 可能包含的值：对象（map[string]any）、数组（[]any、[]string）、
// 字符串、布尔、整数（含值为整数的 float64）与 RawJSON。对象的键按 UTF-16 码元排序；字符串只转义 JCS 要求的字符。
// 不支持非整数：payload 中没有小数。
func Canonical(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := canon(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func canon(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		// 解码后的 JSON 数字（验签方重新规范化时）：只支持 2^53 以内的整数，JCS 输出为不带小数点的整数。
		if x != math.Trunc(x) || math.Abs(x) > 1<<53 {
			return fmt.Errorf("clientconfig: non-integer number %v", x)
		}
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case string:
		return canonString(b, x)
	case RawJSON:
		b.Write(x)
	case []string:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := canonString(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := canon(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := canonString(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := canon(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("clientconfig: unsupported type %T", v)
	}
	return nil
}

// canonString 按 JCS（ECMAScript JSON.stringify）转义：只转义 "、\ 与 U+0000–U+001F。
func canonString(b *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return errors.New("clientconfig: invalid UTF-8")
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return nil
}

// utf16Less 按 UTF-16 码元比较（RFC 8785 3.2.3）。
func utf16Less(a, b string) bool {
	ua, ub := utf16Units(a), utf16Units(b)
	return slices.Compare(ua, ub) < 0
}

func utf16Units(s string) []uint16 {
	var out []uint16
	for _, r := range s {
		if r >= 0x10000 {
			r -= 0x10000
			out = append(out, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
			continue
		}
		out = append(out, uint16(r))
	}
	return out
}

// Platforms 是 ClientPlatform 的取值（spec/30）。
var Platforms = []string{"ios", "android", "windows", "macos", "linux"}

var versionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// ValidVersion 判断 min_version 中的版本号格式（x.y.z，不含前导零）。
func ValidVersion(v string) bool { return versionRE.MatchString(v) }

// Client 是从 User-Agent 解析出的自研客户端版本。
type Client struct {
	Platform string // ClientPlatform 取值；无法识别时为空
	Version  [3]int
}

// UserAgent 按 API-03 解析 User-Agent：只有以 "<appName>/x.y.z" 开头的才是自研客户端；
// 平台取第一个括号内的第一个词，不区分大小写，iPadOS 对应 ios。预发布与构建后缀被忽略。
type UserAgent struct {
	re *regexp.Regexp
}

// tchar 为 RFC 9110 token 允许的字符。
var tokenRE = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// NewUserAgent 用部署配置 client.app_name 建立解析器；appName 必须是 RFC 9110 token。
func NewUserAgent(appName string) (*UserAgent, error) {
	if !tokenRE.MatchString(appName) {
		return nil, fmt.Errorf("clientconfig: app name %q is not an RFC 9110 token", appName)
	}
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(appName) + `/(\d+)\.(\d+)\.(\d+)\S*(?:[^(]*\(\s*([^;\s)]+))?`)
	return &UserAgent{re: re}, nil
}

// Parse 返回自研客户端信息；不是自研客户端（浏览器、第三方客户端）时 ok 为 false。
func (u *UserAgent) Parse(ua string) (c Client, ok bool) {
	m := u.re.FindStringSubmatch(ua)
	if m == nil {
		return Client{}, false
	}
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return Client{}, false
		}
		c.Version[i] = n
	}
	p := strings.ToLower(m[4])
	if p == "ipados" {
		p = "ios"
	}
	if slices.Contains(Platforms, p) {
		c.Platform = p
	}
	return c, true
}

// Outdated 判断客户端是否低于 minVersions 中其平台的最低版本。平台无法识别或未配置时不比较。
func Outdated(c Client, minVersions map[string]string) bool {
	if c.Platform == "" {
		return false
	}
	v, ok := minVersions[c.Platform]
	if !ok || !ValidVersion(v) {
		return false
	}
	var want [3]int
	for i, part := range strings.Split(v, ".") {
		want[i], _ = strconv.Atoi(part)
	}
	return slices.Compare(c.Version[:], want[:]) < 0
}
