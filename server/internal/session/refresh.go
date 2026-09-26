// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/akari-project/panel/server/internal/account"
	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// OAuthError 是 /v1/oauth/token 的错误（RFC 6749 §5.2，CONV-16 的例外）。
// Retry 表示重试窗口内的并发刷新未取得缓存：令牌对已由另一请求取得，浏览器不应清除 Cookie。
type OAuthError struct {
	Code  string
	Retry bool
}

func (e *OAuthError) Error() string { return e.Code }

// ErrInvalidGrant：刷新令牌无效、已过期、已吊销或已被使用。客户端应重新登录。
var ErrInvalidGrant = &OAuthError{Code: "invalid_grant"}

// retryAD 是重试缓存的附加数据；键为旧刷新令牌的哈希（ADR 0017）。
var retryAD = []byte("session.retry-cache")

func retryKey(oldHash string) string { return "auth:rotated:" + oldHash }

// RetryCacheKey 返回刷新令牌 refresh 被轮换后，其新令牌对在 Valkey 中的缓存键（测试用）。
func RetryCacheKey(refresh string) string { return retryKey(refreshHash(refresh)) }

// cachedPair 与 Tokens 字段相同，只多了 JSON 标签。
type cachedPair struct {
	Access          string    `json:"a"`
	Refresh         string    `json:"r"`
	AccessExpires   time.Time `json:"e"`
	RefreshExpires  time.Time `json:"x"`
	AbsoluteExpires time.Time `json:"b"`
	SessionID       uuid.UUID `json:"s"`
	DeviceID        uuid.UUID `json:"d"`
}

// Refresh 轮换刷新令牌（AUTH-07）：
//   - 令牌有效：旧会话标记为已使用，建立子会话（继承绝对失效时间），签发新令牌对；
//   - 已使用的令牌在 10 秒内再次出现，且 IP 前缀与 User-Agent 与子会话相同：返回缓存的同一新令牌对；
//     缓存未命中时返回 invalid_grant，不签发新令牌，也不吊销会话链（ADR 0017）；
//   - 其余情况下再次出现已使用的令牌视为泄露，吊销整条会话链；
//   - 已吊销、已过期、账号不在正常状态或设备已吊销：invalid_grant。
func (s *Service) Refresh(ctx context.Context, refresh, ipPrefix, ua string) (Tokens, error) {
	return s.refresh(ctx, token.AudienceClient, refresh, ipPrefix, ua)
}

// ConsoleRefresh 轮换管理会话的刷新令牌（AUTH-21）：规则与 Refresh 相同，空闲 30 分钟失效，
// 绝对失效时间继承自登录时的 12 小时。客户端会话的刷新令牌在这里无效，反之亦然。
// 管理会话只能由完成二次验证的登录建立，因此轮换后的访问令牌同样带 amr（pwd、otp）。
func (s *Service) ConsoleRefresh(ctx context.Context, refresh, ipPrefix, ua string) (Tokens, error) {
	return s.refresh(ctx, token.AudienceConsole, refresh, ipPrefix, ua)
}

func (s *Service) refresh(ctx context.Context, aud token.Audience, refresh, ipPrefix, ua string) (Tokens, error) {
	if refresh == "" {
		return Tokens{}, &OAuthError{Code: "invalid_request"}
	}
	hash := refreshHash(refresh)
	var (
		out     Tokens
		parent  uuid.UUID
		revoked []uuid.UUID
		reused  bool
	)
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		sess, err := q.SessionByRefreshHash(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidGrant
		}
		if err != nil {
			return err
		}
		now := s.Clock.Now()
		if sess.RevokedAt != nil {
			return ErrInvalidGrant
		}
		// 两种会话的刷新令牌只能在各自的接口使用（AUTH-21）：先于重试窗口与泄露判定检查，
		// 另一接口上出现的令牌既不能取得缓存的令牌对，也不能触发对方会话链的吊销。
		if sess.Audience != string(aud) {
			return ErrInvalidGrant
		}
		if sess.UsedAt != nil {
			if now.Sub(*sess.UsedAt) < RetryWindow {
				child, err := q.ChildSession(ctx, &sess.ID)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				if err == nil && deref(child.IpPrefix) == ipPrefix && deref(child.UserAgent) == ua {
					reused = true
					return nil
				}
			}
			// 泄露：吊销整条链（AUTH-07）。吊销需要提交，因此不以错误结束事务。
			revoked, err = revokeChain(ctx, q, sess.ID, now)
			return err
		}
		absolute := sess.ExpiresAt
		if sess.AbsoluteExpiresAt != nil {
			absolute = *sess.AbsoluteExpiresAt
		}
		if !now.Before(sess.ExpiresAt) || !now.Before(absolute) || sess.AccountStatus != "active" || sess.DeviceRevokedAt != nil {
			return ErrInvalidGrant
		}
		idle, device, amr := ClientIdle, uuid.Nil, []string(nil)
		if aud == token.AudienceConsole {
			idle, amr = ConsoleIdle, ConsoleAMR
		} else if sess.DeviceID == nil {
			return ErrInvalidGrant
		} else {
			device = *sess.DeviceID
		}
		if err := q.MarkSessionUsed(ctx, sqlc.MarkSessionUsedParams{ID: sess.ID, Now: &now}); err != nil {
			return err
		}
		if device != uuid.Nil {
			// 设备最近活跃时间（AUTH-14 的排序依据，设备列表显示）。
			if err := q.TouchDeviceSeen(ctx, sqlc.TouchDeviceSeenParams{ID: device, Now: &now}); err != nil {
				return err
			}
		}
		parent = sess.ID
		out, err = s.newSession(ctx, q, sess.AccountID, device, aud, &sess.ID, ua, ipPrefix, idle, absolute, amr)
		if err != nil {
			return err
		}
		// 在提交之前写入重试缓存：并发的同一令牌请求在行锁释放后必定能读到（ADR 0017）。写入失败则回滚。
		if err := s.cachePair(ctx, hash, out); err != nil {
			return apierr.Unavailable(err)
		}
		return nil
	})
	if err != nil {
		return Tokens{}, err
	}
	switch {
	case reused:
		return s.cachedRetry(ctx, hash)
	case len(revoked) > 0:
		if err := s.revokeAfterCommit(ctx, revoked); err != nil {
			return Tokens{}, apierr.Unavailable(err)
		}
		return Tokens{}, ErrInvalidGrant
	case out.Access == "":
		return Tokens{}, ErrInvalidGrant
	}
	if aud == token.AudienceClient {
		s.carryReauth(ctx, parent, out.SessionID)
	}
	return out, nil
}

// carryReauth 把上一会话的重新验证状态连同剩余有效期复制到轮换后的会话（AUTH-23 的 5 分钟窗口按会话链计算）。
// 复制失败时新会话需要重新验证，不影响刷新本身。
func (s *Service) carryReauth(ctx context.Context, from, to uuid.UUID) {
	_ = s.kv(ctx, s.KV.B().Copy().Source(reauthKey(from)).Destination(reauthKey(to)).Build())[0].Error()
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// cachePair 把新令牌对加密后缓存 10 秒（CONV-19、CONV-20 例外，ADR 0017）。
func (s *Service) cachePair(ctx context.Context, oldHash string, t Tokens) error {
	pt, err := json.Marshal(cachedPair(t))
	if err != nil {
		return err
	}
	ct, err := s.Keys.Seal(pt, retryAD)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, auth.Timeout)
	defer cancel()
	return s.KV.Do(ctx, s.KV.B().Set().Key(retryKey(oldHash)).Value(string(ct)).Px(RetryWindow).Build()).Error()
}

// revokeChain 吊销会话所在的整条链。与之并发的轮换可能在语句快照之后插入子会话，
// 因此重复执行直到没有新吊销的行。
func revokeChain(ctx context.Context, q *sqlc.Queries, id uuid.UUID, now time.Time) ([]uuid.UUID, error) {
	var all []uuid.UUID
	for range 10 {
		ids, err := q.RevokeSessionChain(ctx, sqlc.RevokeSessionChainParams{ID: id, Now: &now})
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			break
		}
		all = append(all, ids...)
	}
	return all, nil
}

func (s *Service) cachedRetry(ctx context.Context, oldHash string) (Tokens, error) {
	ctx, cancel := context.WithTimeout(ctx, auth.Timeout)
	defer cancel()
	raw, err := s.KV.Do(ctx, s.KV.B().Get().Key(retryKey(oldHash)).Build()).AsBytes()
	if err != nil {
		return Tokens{}, &OAuthError{Code: "invalid_grant", Retry: true}
	}
	pt, err := s.Keys.Open(raw, retryAD)
	if err != nil {
		return Tokens{}, ErrInvalidGrant
	}
	var c cachedPair
	if err := json.Unmarshal(pt, &c); err != nil {
		return Tokens{}, ErrInvalidGrant
	}
	return Tokens(c), nil
}

// Logout 吊销当前会话所在的会话链，并吊销本设备及其代理凭据，释放设备名额（AUTH-10）；
// 因上限等待凭据的设备随即取得空出的名额（AUTH-14）。
func (s *Service) Logout(ctx context.Context, p auth.Principal) error {
	var revoked []uuid.UUID
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		now := s.Clock.Now()
		if _, err := q.LockAccount(ctx, p.AccountID); err != nil {
			return err
		}
		device, err := q.SessionDevice(ctx, sqlc.SessionDeviceParams{ID: p.SessionID, AccountID: p.AccountID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.Unauthenticated
		}
		if err != nil {
			return err
		}
		if revoked, err = revokeChain(ctx, q, p.SessionID, now); err != nil {
			return err
		}
		if device == nil {
			return nil
		}
		if err := q.RevokeDevice(ctx, sqlc.RevokeDeviceParams{ID: *device, Now: &now}); err != nil {
			return err
		}
		more, err := q.RevokeDeviceSessions(ctx, sqlc.RevokeDeviceSessionsParams{DeviceID: device, Now: &now})
		if err != nil {
			return err
		}
		revoked = append(revoked, more...)
		creds, err := q.RevokeDeviceCredentials(ctx, sqlc.RevokeDeviceCredentialsParams{DeviceID: device, Now: &now})
		if err != nil {
			return err
		}
		for _, c := range creds {
			if err := credentialRevoked(ctx, q, p.AccountID, c); err != nil {
				return err
			}
		}
		return account.ReconcileCredentials(ctx, q, s.Keys, p.AccountID, now)
	})
	if err != nil {
		return err
	}
	// 当前会话即使已不在数据库的未吊销集合中，也写入吊销集合，使访问令牌立即失效。
	revoked = append(revoked, p.SessionID)
	if err := s.revokeAfterCommit(ctx, revoked); err != nil {
		return apierr.Unavailable(err)
	}
	return nil
}

func credentialRevoked(ctx context.Context, q *sqlc.Queries, acct, credential uuid.UUID) error {
	return account.CredentialChanged(ctx, q, acct, credential, "revoked")
}
