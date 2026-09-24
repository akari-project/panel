// SPDX-License-Identifier: AGPL-3.0-or-later

package mfa

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238 的默认算法，验证器应用普遍只支持 SHA-1
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"time"
)

// TOTP 参数（RFC 6238）：HMAC-SHA1，30 秒一步，6 位。验证时接受前后各一步的时钟偏差。
const (
	Period     = 30 * time.Second
	Digits     = 6
	SecretSize = 20
	skew       = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// EncodeSecret 返回密钥的 Base32（无填充）。
func EncodeSecret(secret []byte) string { return b32.EncodeToString(secret) }

// Step 返回时刻 t 所在的时间步。
func Step(t time.Time) int64 { return t.Unix() / int64(Period/time.Second) }

// Code 计算时间步 step 的验证码（RFC 4226 动态截断）。
func Code(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", Digits, v%1_000_000)
}

// Match 在 now 前后各一步内查找与 code 相符、且晚于 lastStep 的时间步（AUTH-11：同一时间步内
// 已使用过的码拒绝再次使用）。找到时返回该时间步。
func Match(secret []byte, code string, now time.Time, lastStep *int64) (int64, bool) {
	cur := Step(now)
	for s := cur - skew; s <= cur+skew; s++ {
		if lastStep != nil && s <= *lastStep {
			continue
		}
		if hmac.Equal([]byte(Code(secret, s)), []byte(code)) {
			return s, true
		}
	}
	return 0, false
}

// URI 返回供验证器应用扫描的 otpauth URI。
func URI(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", EncodeSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(Digits))
	q.Set("period", fmt.Sprint(int(Period/time.Second)))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
