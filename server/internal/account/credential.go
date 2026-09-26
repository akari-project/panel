// SPDX-License-Identifier: AGPL-3.0-or-later

package account

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/secretbox"
)

// CredentialSecretAD 是 proxy_credentials.secret_enc 的附加数据。
var CredentialSecretAD = []byte("proxy_credentials.secret_enc")

// OpenCredentialSecret 解密 proxy_credentials.secret_enc，返回 16 字节 UUIDv4 形式的凭据 secret（spec/21 AGT-15）。
// 早期 `panel admin create` 写入的 36 字符文本形式同样接受并解析为 16 字节；其他长度或格式一律拒绝。
func OpenCredentialSecret(keys *secretbox.Keyring, enc []byte) (uuid.UUID, error) {
	pt, err := keys.Open(enc, CredentialSecretAD)
	if err != nil {
		return uuid.Nil, err
	}
	switch len(pt) {
	case 16:
		return uuid.UUID(pt), nil
	case 36:
		if id, err := uuid.Parse(string(pt)); err == nil {
			return id, nil
		}
	}
	return uuid.Nil, errBadSecret
}

var errBadSecret = errors.New("account: credential secret is neither 16 raw bytes nor a 36-character UUID")

// CreateSharedCredential 为账号生成第三方客户端共用的代理凭据（AUTH-13），并在同一事务中写入
// credential.changed 事件（CONV-22，载荷见 CONV-34）。凭据明文为 16 字节原始 UUIDv4（spec/21 AGT-15），
// 加密保存（CONV-19）。
func CreateSharedCredential(ctx context.Context, q *sqlc.Queries, keys *secretbox.Keyring, account uuid.UUID) (uuid.UUID, error) {
	return newSharedCredential(ctx, q, keys, account, "created")
}

// RotateSharedCredential 为账号生成新的共用凭据并写 credential.changed（change 为 rotated）。
// 调用方已在同一事务中吊销旧的共用凭据（并为其写 revoked）。
func RotateSharedCredential(ctx context.Context, q *sqlc.Queries, keys *secretbox.Keyring, account uuid.UUID) (uuid.UUID, error) {
	return newSharedCredential(ctx, q, keys, account, "rotated")
}

func newSharedCredential(ctx context.Context, q *sqlc.Queries, keys *secretbox.Keyring, account uuid.UUID, change string) (uuid.UUID, error) {
	secret := uuid.New()
	enc, err := keys.Seal(secret[:], CredentialSecretAD)
	if err != nil {
		return uuid.Nil, err
	}
	id, err := q.CreateSharedCredential(ctx, sqlc.CreateSharedCredentialParams{AccountID: account, SecretEnc: enc})
	if err != nil {
		return uuid.Nil, err
	}
	return id, CredentialChanged(ctx, q, account, id, change)
}

// CredentialChanged 写入 credential.changed 事件；change 取 created、rotated、revoked（CONV-34）。
func CredentialChanged(ctx context.Context, q *sqlc.Queries, account, credential uuid.UUID, change string) error {
	payload, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"account_id":     account,
		"credential_id":  credential,
		"change":         change,
	})
	if err != nil {
		return err
	}
	_, err = q.InsertOutboxEvent(ctx, sqlc.InsertOutboxEventParams{Topic: "credential.changed", Payload: payload, SchemaVersion: 1})
	return err
}

// 邀请码字母表去除易混字符（0/O、1/I/L）。
const referralAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

var errReferralCollision = errors.New("account: could not allocate a unique referral code")

// UniqueReferralCode 生成一个未被占用的 8 位账号邀请码。
func UniqueReferralCode(ctx context.Context, q *sqlc.Queries) (string, error) {
	for range 8 {
		code := randomCode(8)
		exists, err := q.ReferralCodeExists(ctx, code)
		if err != nil {
			return "", err
		}
		if !exists {
			return code, nil
		}
	}
	return "", errReferralCollision
}

func randomCode(n int) string {
	b := make([]byte, n)
	out := make([]byte, n)
	for i := 0; i < n; {
		if _, err := rand.Read(b); err != nil {
			panic(fmt.Sprintf("account: crypto/rand: %v", err))
		}
		for _, x := range b {
			// 拒绝采样，避免取模偏差。
			if int(x) >= 256-256%len(referralAlphabet) {
				continue
			}
			out[i] = referralAlphabet[int(x)%len(referralAlphabet)]
			i++
			if i == n {
				break
			}
		}
	}
	return string(out)
}
