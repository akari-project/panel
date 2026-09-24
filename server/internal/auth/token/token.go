// SPDX-License-Identifier: AGPL-3.0-or-later

// Package token 签发与校验访问令牌（spec/10 AUTH-06）：PASETO v4.public，有效期 15 分钟，
// 页脚带 key id，声明中带会话 ID sid 与受众 aud（client 或 console），管理令牌另带 amr（AUTH-21）。
//
// 签名私钥不入库，与主密钥一样来自环境变量（CONV-30）：当前密钥 PANEL_TOKEN_KEY，
// 轮换期间的旧密钥 PANEL_TOKEN_KEY_PREVIOUS，格式都是 "<key_id>:<base64 的 32 字节 Ed25519 种子>"，
// key_id 为 1–255。旧密钥只用于验签，轮换后保留 30 分钟（访问令牌最长有效期的两倍）再下线。
package token

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"aidanwoods.dev/go-paseto"
	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/clock"
)

// TTL 是访问令牌有效期（AUTH-06）。
const TTL = 15 * time.Minute

// Audience 是令牌受众。
type Audience string

// 受众取值。
const (
	AudienceClient  Audience = "client"
	AudienceConsole Audience = "console"
)

// Claims 是访问令牌携带的声明。
type Claims struct {
	AccountID uuid.UUID
	SessionID uuid.UUID
	Audience  Audience
	// AMR 是本次登录完成的认证方式（RFC 8176），管理令牌必须含二次验证（AUTH-21）。
	AMR       []string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// ErrInvalid 表示令牌无效：格式错误、签名不符、受众不符、已过期或 key id 未知。
// 调用方一律返回 401 unauthenticated，不区分原因。
var ErrInvalid = errors.New("token: invalid")

type key struct {
	id string
	sk paseto.V4AsymmetricSecretKey
	pk paseto.V4AsymmetricPublicKey
}

// Keyring 持有签名密钥与验签公钥。
type Keyring struct {
	clk     clock.Clock
	current key
	byID    map[string]key
}

// parseKey 解析 "<key_id>:<base64 的 32 字节 Ed25519 种子>"。
func parseKey(s string) (key, error) {
	id, b64, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return key{}, errors.New("token: key must be <key_id>:<base64>")
	}
	n, err := strconv.Atoi(id)
	if err != nil || n < 1 || n > 255 {
		return key{}, errors.New("token: key_id must be 1–255")
	}
	seed, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		seed, err = base64.RawURLEncoding.DecodeString(b64)
	}
	if err != nil || len(seed) != ed25519.SeedSize {
		return key{}, fmt.Errorf("token: key must be %d bytes of base64", ed25519.SeedSize)
	}
	sk, err := paseto.NewV4AsymmetricSecretKeyFromSeed(hex.EncodeToString(seed))
	if err != nil {
		return key{}, fmt.Errorf("token: %w", err)
	}
	return key{id: strconv.Itoa(n), sk: sk, pk: sk.Public()}, nil
}

// NewKeyring 从环境变量形式的当前密钥与可选的旧密钥建立密钥环。
func NewKeyring(clk clock.Clock, current, previous string) (*Keyring, error) {
	if current == "" {
		return nil, errors.New("token: PANEL_TOKEN_KEY is not set")
	}
	cur, err := parseKey(current)
	if err != nil {
		return nil, err
	}
	k := &Keyring{clk: clk, current: cur, byID: map[string]key{cur.id: cur}}
	if previous != "" {
		prev, err := parseKey(previous)
		if err != nil {
			return nil, err
		}
		if prev.id == cur.id {
			return nil, errors.New("token: previous key must have a different key_id")
		}
		k.byID[prev.id] = prev
	}
	return k, nil
}

type footer struct {
	KID string `json:"kid"`
}

// Issue 签发访问令牌。IssuedAt 与 ExpiresAt 由注入的时钟决定（CONV-04）。
func (k *Keyring) Issue(c Claims) (string, Claims, error) {
	now := k.clk.Now().Truncate(time.Second)
	c.IssuedAt, c.ExpiresAt = now, now.Add(TTL)
	t := paseto.NewToken()
	t.SetSubject(c.AccountID.String())
	t.SetString("sid", c.SessionID.String())
	t.SetAudience(string(c.Audience))
	t.SetIssuedAt(c.IssuedAt)
	t.SetExpiration(c.ExpiresAt)
	if len(c.AMR) > 0 {
		if err := t.Set("amr", c.AMR); err != nil {
			return "", Claims{}, err
		}
	}
	f, err := json.Marshal(footer{KID: k.current.id})
	if err != nil {
		return "", Claims{}, err
	}
	t.SetFooter(f)
	return t.V4Sign(k.current.sk, nil), c, nil
}

// Verify 校验令牌并要求受众为 aud。
func (k *Keyring) Verify(tok string, aud Audience) (Claims, error) {
	p := paseto.MakeParser(nil)
	raw, err := p.UnsafeParseFooter(paseto.V4Public, tok)
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var f footer
	if err := json.Unmarshal(raw, &f); err != nil {
		return Claims{}, ErrInvalid
	}
	key, ok := k.byID[f.KID]
	if !ok {
		return Claims{}, ErrInvalid
	}
	t, err := p.ParseV4Public(key.pk, tok, nil)
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var c Claims
	sub, err1 := t.GetSubject()
	sid, err2 := t.GetString("sid")
	a, err3 := t.GetAudience()
	iat, err4 := t.GetIssuedAt()
	exp, err5 := t.GetExpiration()
	if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
		return Claims{}, ErrInvalid
	}
	if c.AccountID, err = uuid.Parse(sub); err != nil {
		return Claims{}, ErrInvalid
	}
	if c.SessionID, err = uuid.Parse(sid); err != nil {
		return Claims{}, ErrInvalid
	}
	c.Audience, c.IssuedAt, c.ExpiresAt = Audience(a), iat, exp
	if c.Audience != aud {
		return Claims{}, ErrInvalid
	}
	if !k.clk.Now().Before(c.ExpiresAt) {
		return Claims{}, ErrInvalid
	}
	if _, has := t.Claims()["amr"]; has {
		if err := t.Get("amr", &c.AMR); err != nil {
			return Claims{}, ErrInvalid
		}
	}
	return c, nil
}
