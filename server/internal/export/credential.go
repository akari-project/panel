// SPDX-License-Identifier: AGPL-3.0-or-later

// Package export 生成第三方客户端的导出配置（spec/23）。
package export

import (
	"crypto/hkdf"
	"crypto/sha256"

	"github.com/google/uuid"
)

// UUIDText 是凭据 secret（16 字节 UUIDv4）按 RFC 9562 的标准文本形式（EXP-09 uuid_text）：
// 32 个小写十六进制数字按 8-4-4-4-12 以连字符分隔。VLESS、VMess、TUIC 的 uuid 与各协议的密码均取此值。
func UUIDText(secret uuid.UUID) string { return secret.String() }

// SS2022Key16 是 Shadowsocks 2022 2022-blake3-aes-128-gcm 的用户密钥（EXP-09）：
// HKDF-SHA256(ikm = secret, salt = 空, info = "akari-ss2022-user-key-16-v1", L = 16)。
func SS2022Key16(secret uuid.UUID) []byte {
	return ss2022Key(secret, "akari-ss2022-user-key-16-v1", 16)
}

// SS2022Key32 是 2022-blake3-aes-256-gcm 的用户密钥（EXP-09），info 为 "akari-ss2022-user-key-32-v1"，L = 32。
func SS2022Key32(secret uuid.UUID) []byte {
	return ss2022Key(secret, "akari-ss2022-user-key-32-v1", 32)
}

func ss2022Key(secret uuid.UUID, info string, n int) []byte {
	k, err := hkdf.Key(sha256.New, secret[:], nil, info, n)
	if err != nil {
		panic(err) // 只在 n 超出 HKDF 上限时发生
	}
	return k
}
