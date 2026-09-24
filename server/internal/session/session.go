// SPDX-License-Identifier: AGPL-3.0-or-later

// Package session 是登录、会话与刷新令牌（spec/10 10.2）：
//
//   - 登录校验密码，执行限流与冷却（AUTH-09），注册设备（AUTH-10），决定本设备代理凭据的状态（AUTH-14）；
//   - 会话保存刷新令牌的 SHA-256（CONV-20），每次使用即轮换；已轮换的令牌再次出现时吊销整条会话链，
//     10 秒内来自同一 IP 前缀与 User-Agent 的重试返回同一新令牌对（AUTH-07、ADR 0017）；
//   - 访问令牌为 PASETO v4.public（AUTH-06），会话吊销时 sid 写入 Valkey 吊销集合。
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go"

	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/mfa"
	"github.com/akari-project/panel/server/internal/notify"
	"github.com/akari-project/panel/server/internal/password"
	"github.com/akari-project/panel/server/internal/ratelimit"
	"github.com/akari-project/panel/server/internal/secretbox"
)

// 客户端会话的有效期（AUTH-07）：空闲 30 天失效，自首次登录起 90 天绝对失效。
const (
	ClientIdle     = 30 * 24 * time.Hour
	ClientAbsolute = 90 * 24 * time.Hour
	// RetryWindow 是已轮换刷新令牌的重试窗口（AUTH-07）。
	RetryWindow = 10 * time.Second
	// MaxWebDevices 是每个账号保留的未吊销 web 设备数（AUTH-10）。
	MaxWebDevices = 50
	// NonceTTL 是设备复用 nonce 的有效期（AUTH-10）。
	NonceTTL = 60 * time.Second
)

// LoginPerIP 是同一 IP 的登录尝试上限（AUTH-09）。按账号的失败冷却见 attempts.go。
var LoginPerIP = ratelimit.Rule{Name: "login-ip", Limit: 20, Window: time.Minute}

// Service 实现登录、刷新与登出。
type Service struct {
	Pool        *pgxpool.Pool
	KV          valkey.Client
	Clock       clock.Clock
	Keys        *secretbox.Keyring
	Tokens      *token.Keyring
	Revocations auth.Revocations
	Limiter     ratelimit.Limiter
	// MFA 校验登录第二步与重新验证的 TOTP 码或恢复码（AUTH-20、AUTH-23）。
	MFA *mfa.Service
	// Outbox 写入安全通知（OPS-04，例如密码已修改）。
	Outbox notify.Outbox
	// Verify 校验密码，默认 password.Verify。测试替换它来确认两条登录路径都恰好调用一次（AUTH-09）。
	Verify func(pw, encoded string) (bool, error)
}

func (s *Service) verify(pw, encoded string) (bool, error) {
	if s.Verify != nil {
		return s.Verify(pw, encoded)
	}
	return password.Verify(pw, encoded)
}

// DummyHash 是账号不存在时用于校验的固定 argon2id 哈希，参数与默认参数相同，
// 使两条路径的耗时一致（AUTH-09）。对应的密码不存在于任何账号。
var DummyHash = func() string {
	h, err := password.Hash("akari-dummy-password-never-matches", password.DefaultParams)
	if err != nil {
		panic(err)
	}
	return h
}()

// Tokens 是签发给客户端的一对令牌。
type Tokens struct {
	Access, Refresh string
	AccessExpires   time.Time
	// RefreshExpires 是刷新令牌的失效时间（空闲期限，不超过会话链的绝对失效时间）。
	RefreshExpires time.Time
	SessionID      uuid.UUID
	DeviceID       uuid.UUID
}

func newRefreshToken() (plain, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	plain = base64.RawURLEncoding.EncodeToString(b)
	return plain, refreshHash(plain), nil
}

func refreshHash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// newSession 在事务中建立会话并签发令牌对。parent 为轮换前的会话，absolute 为会话链的绝对失效时间。
func (s *Service) newSession(ctx context.Context, q *sqlc.Queries, account, device uuid.UUID, aud token.Audience,
	parent *uuid.UUID, ua, ipPrefix string, idle time.Duration, absolute time.Time, amr []string) (Tokens, error) {
	now := s.Clock.Now()
	id, err := uuid.NewV7()
	if err != nil {
		return Tokens{}, err
	}
	plain, hash, err := newRefreshToken()
	if err != nil {
		return Tokens{}, err
	}
	expires := now.Add(idle)
	if expires.After(absolute) {
		expires = absolute
	}
	dev := device
	if err := q.InsertSession(ctx, sqlc.InsertSessionParams{
		ID: id, AccountID: account, DeviceID: &dev, Audience: string(aud), RefreshTokenHash: hash, ParentID: parent,
		UserAgent: nilIfEmpty(ua), IpPrefix: nilIfEmpty(ipPrefix), ExpiresAt: expires, AbsoluteExpiresAt: &absolute,
	}); err != nil {
		return Tokens{}, err
	}
	access, claims, err := s.Tokens.Issue(token.Claims{AccountID: account, SessionID: id, Audience: aud, AMR: amr})
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{Access: access, Refresh: plain, AccessExpires: claims.ExpiresAt, RefreshExpires: expires, SessionID: id, DeviceID: device}, nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// revokeAfterCommit 把已吊销的会话写入吊销集合。数据库已提交，Valkey 失败时返回错误以便调用方报告 503；
// 访问令牌最长 15 分钟后自然失效。
func (s *Service) revokeAfterCommit(ctx context.Context, sids []uuid.UUID) error {
	return s.Revocations.Revoke(context.WithoutCancel(ctx), sids...)
}

// RevokeAccount 在调用方的事务中吊销账号的全部会话，返回会话 ID（找回密码成功后，AUTH-04）。
// 调用方提交事务后调用 AfterRevoke。
func (s *Service) RevokeAccount(ctx context.Context, tx pgx.Tx, account uuid.UUID) ([]uuid.UUID, error) {
	now := s.Clock.Now()
	return sqlc.New(tx).RevokeAccountSessions(ctx, sqlc.RevokeAccountSessionsParams{AccountID: account, Now: &now})
}

// AfterRevoke 把会话写入吊销集合。
func (s *Service) AfterRevoke(ctx context.Context, sids []uuid.UUID) error {
	return s.revokeAfterCommit(ctx, sids)
}
