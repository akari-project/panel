// SPDX-License-Identifier: AGPL-3.0-or-later

// Package password 实现 argon2id 密码哈希（spec/10 AUTH-01）。
// 编码格式与 PHC 字符串一致：$argon2id$v=19$m=<KiB>,t=<迭代>,p=<并行>$<盐>$<哈希>。
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// 长度限制（AUTH-01），按字符计。
const (
	MinLength = 8
	MaxLength = 128
)

// Params 是 argon2id 参数。
type Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
}

// DefaultParams 为内存 64 MiB、迭代 3、并行 1（AUTH-01）。
var DefaultParams = Params{MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 1}

const (
	saltLen = 16
	keyLen  = 32
)

// ErrLength 表示密码长度不在 8–128 之间。
var ErrLength = fmt.Errorf("password: length must be between %d and %d characters", MinLength, MaxLength)

// CheckLength 校验密码长度。
func CheckLength(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < MinLength || n > MaxLength {
		return ErrLength
	}
	return nil
}

// Hash 返回 pw 的 argon2id 编码哈希。
func Hash(pw string, p Params) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(pw), salt, p.Iterations, p.MemoryKiB, p.Parallelism, keyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism,
		enc.EncodeToString(salt), enc.EncodeToString(sum)), nil
}

// Verify 以常数时间比较 pw 与编码哈希。
func Verify(pw, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("password: unsupported hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("password: unsupported argon2 version")
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Iterations, &p.Parallelism); err != nil {
		return false, errors.New("password: bad parameters")
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false, errors.New("password: bad salt")
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil {
		return false, errors.New("password: bad hash")
	}
	got := argon2.IDKey([]byte(pw), salt, p.Iterations, p.MemoryKiB, p.Parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
