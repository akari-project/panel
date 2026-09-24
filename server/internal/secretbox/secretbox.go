// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secretbox 实现应用层 AEAD 加密，用于 `_enc` 列与其他需要加密的敏感值（CONV-19）。
//
// 密文格式为 key_id（1 字节）‖ nonce（24 字节）‖ ciphertext（CONV-30），算法为 XChaCha20-Poly1305。
// 运行时同时加载当前主密钥与至多一把旧主密钥：加密只用当前密钥，解密按 key_id 选择。
package secretbox

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// KeySize 是主密钥长度。
const KeySize = chacha20poly1305.KeySize

// ErrDecrypt 表示密文无法解密（密钥未加载、被篡改或格式错误）。错误信息不含密文与密钥。
var ErrDecrypt = errors.New("secretbox: cannot decrypt")

// Key 是一把带 ID 的主密钥。
type Key struct {
	ID  byte
	Raw []byte
}

// ParseKey 解析 "<key_id>:<base64 的 32 字节密钥>"，key_id 为 1–255。
func ParseKey(s string) (Key, error) {
	id, b64, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return Key{}, errors.New("secretbox: key must be <key_id>:<base64>")
	}
	n, err := strconv.Atoi(id)
	if err != nil || n < 1 || n > 255 {
		return Key{}, errors.New("secretbox: key_id must be 1–255")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(b64)
	}
	if err != nil || len(raw) != KeySize {
		return Key{}, fmt.Errorf("secretbox: key must be %d bytes of base64", KeySize)
	}
	return Key{ID: byte(n), Raw: raw}, nil
}

// Keyring 持有当前主密钥与可选的旧主密钥。
type Keyring struct {
	current byte
	aeads   map[byte]aead
}

type aead interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	NonceSize() int
}

// NewKeyring 以 current 为加密密钥；previous 可为 nil。
func NewKeyring(current Key, previous *Key) (*Keyring, error) {
	k := &Keyring{current: current.ID, aeads: map[byte]aead{}}
	add := func(key Key) error {
		a, err := chacha20poly1305.NewX(key.Raw)
		if err != nil {
			return fmt.Errorf("secretbox: %w", err)
		}
		k.aeads[key.ID] = a
		return nil
	}
	if err := add(current); err != nil {
		return nil, err
	}
	if previous != nil {
		if previous.ID == current.ID {
			return nil, errors.New("secretbox: previous key must have a different key_id")
		}
		if err := add(*previous); err != nil {
			return nil, err
		}
	}
	return k, nil
}

// ParseKeyring 从环境变量形式的两个字符串建立密钥环；previous 可为空。
func ParseKeyring(current, previous string) (*Keyring, error) {
	if current == "" {
		return nil, errors.New("secretbox: PANEL_MASTER_KEY is not set")
	}
	cur, err := ParseKey(current)
	if err != nil {
		return nil, err
	}
	var prev *Key
	if previous != "" {
		p, err := ParseKey(previous)
		if err != nil {
			return nil, err
		}
		prev = &p
	}
	return NewKeyring(cur, prev)
}

// Seal 用当前主密钥加密。ad 为附加数据（例如 "表.列"），解密时必须相同。
func (k *Keyring) Seal(plaintext, ad []byte) ([]byte, error) {
	a := k.aeads[k.current]
	out := make([]byte, 1+a.NonceSize(), 1+a.NonceSize()+len(plaintext)+chacha20poly1305.Overhead)
	out[0] = k.current
	if _, err := rand.Read(out[1:]); err != nil {
		return nil, fmt.Errorf("secretbox: nonce: %w", err)
	}
	return a.Seal(out, out[1:], plaintext, ad), nil
}

// Open 解密由 Seal 产生的密文。
func (k *Keyring) Open(ciphertext, ad []byte) ([]byte, error) {
	if len(ciphertext) < 1 {
		return nil, ErrDecrypt
	}
	a, ok := k.aeads[ciphertext[0]]
	if !ok || len(ciphertext) < 1+a.NonceSize()+chacha20poly1305.Overhead {
		return nil, ErrDecrypt
	}
	ns := a.NonceSize()
	pt, err := a.Open(nil, ciphertext[1:1+ns], ciphertext[1+ns:], ad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// NeedsRotation 报告密文是否不是用当前主密钥加密的（供 `panel keys rotate` 使用）。
func (k *Keyring) NeedsRotation(ciphertext []byte) bool {
	return len(ciphertext) == 0 || ciphertext[0] != k.current
}
