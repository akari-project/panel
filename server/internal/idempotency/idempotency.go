// SPDX-License-Identifier: AGPL-3.0-or-later

// Package idempotency 实现 Idempotency-Key（spec/02 CONV-12）：
//
//   - 作用域：已认证请求为“账号 + 键”，未认证请求为“路由 + 键”；
//   - 保存键、路由、请求体摘要（HMAC-SHA256，见 Store.HashKey）与最终响应，保存 24 小时；
//   - 相同的键与请求体返回相同结果；键相同而请求体或路由不同返回 422 idempotency_key_reused；
//   - 首个请求仍在处理时返回 409 conflict 并带 Retry-After；
//   - 缓存 2xx 与 4xx；5xx、401、429 删除记录，允许用同一键重试。401 与 429 反映调用方当时的认证状态
//     或配额（如处理器返回的 mfa_required、AUTH-09 的限流），重新验证或等待 Retry-After 后原键重试必须成功；
//     鉴权与通用限流先于本中间件执行，这里覆盖的是处理器内部产生的 401 与 429。
//     因此处理器返回 401 或 429 之前不得提交任何副作用（写库、outbox、通知队列）：要么在副作用之前检查，
//     要么在同一事务中返回错误使其回滚，否则同键重试会重复执行。限流计数本身不算副作用；
//   - 过期记录由 worker 每小时清理（Sweep）。
//
// 响应中含秘密值的接口不接受此请求头（CONV-12），由契约决定哪些操作使用本中间件。
package idempotency

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// Header 是请求头名。
const Header = "Idempotency-Key"

// TTL 是记录保存时间。
const TTL = 24 * time.Hour

// Abandoned 是处理中记录的最长存活时间：超过后视为首个请求所在的进程已退出，允许重新执行。
// 抢占时间由 expires_at − TTL 得出（应用时钟写入，CONV-27），接口请求不会运行这么久。
const Abandoned = time.Minute

// Store 保存幂等记录。
type Store struct {
	Pool  *pgxpool.Pool
	Clock clock.Clock
	// HashKey 是请求体摘要的 HMAC 密钥。请求体可能含密码或重置令牌，只存不加密钥的 SHA-256
	// 会在数据库泄露时允许离线字典攻击，因此 request_hash 为 HMAC-SHA256(HashKey, 请求体)。
	HashKey []byte
}

// 重放时不写回的响应头：每次请求各自的值。
var skipHeaders = map[string]bool{"Request-Id": true, "Set-Cookie": true, "Date": true}

// Serve 处理带 Idempotency-Key 的请求；没有该请求头时直接调用 next。
// route 为路由模板；account 为认证主体，未认证时为 nil。错误经 fail 写出。
func (s Store) Serve(w http.ResponseWriter, r *http.Request, route string, account *uuid.UUID, next http.Handler, fail func(http.ResponseWriter, *http.Request, error)) {
	raw := r.Header.Get(Header)
	if raw == "" {
		next.ServeHTTP(w, r)
		return
	}
	key, err := uuid.Parse(raw)
	if err != nil {
		fail(w, r, apierr.Invalid(apierr.Field(Header, "invalid_format")))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, r, apierr.PayloadTooLarge)
			return
		}
		fail(w, r, apierr.Invalid())
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	mac := hmac.New(sha256.New, s.HashKey)
	mac.Write(body)
	sum := mac.Sum(nil)

	ctx := r.Context()
	q := sqlc.New(s.Pool)
	// 抢占失败而记录随即被删除（5xx 或过期）时重试；次数有限，避免与并发请求无限竞争。
	for range 3 {
		id, err := q.ClaimIdempotencyKey(ctx, sqlc.ClaimIdempotencyKeyParams{
			Key: key, AccountID: account, Route: route, RequestHash: sum, ExpiresAt: s.Clock.Now().Add(TTL),
		})
		if err == nil {
			s.run(w, r, id, next, fail)
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			fail(w, r, apierr.Unavailable(err))
			return
		}
		row, err := q.GetIdempotencyKey(ctx, sqlc.GetIdempotencyKeyParams{Key: key, AccountID: account, Route: route})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			fail(w, r, apierr.Unavailable(err))
			return
		}
		now := s.Clock.Now()
		abandoned := row.ResponseStatus == nil && !now.Before(row.ExpiresAt.Add(-TTL+Abandoned))
		if !now.Before(row.ExpiresAt) || abandoned {
			if err := q.DeleteIdempotencyKey(ctx, row.ID); err != nil {
				fail(w, r, apierr.Unavailable(err))
				return
			}
			continue
		}
		if row.Route != route || !hmac.Equal(row.RequestHash, sum) {
			fail(w, r, apierr.IdempotencyKeyReused)
			return
		}
		if row.ResponseStatus == nil {
			e := *apierr.Conflict
			e.RetryAfter = time.Second
			fail(w, r, &e)
			return
		}
		replay(w, row)
		return
	}
	e := *apierr.Conflict
	e.RetryAfter = time.Second
	fail(w, r, &e)
}

func (s Store) run(w http.ResponseWriter, r *http.Request, id uuid.UUID, next http.Handler, fail func(http.ResponseWriter, *http.Request, error)) {
	q := sqlc.New(s.Pool)
	// 记录的写入不随请求取消：客户端断开后，已经执行的业务结果仍要保存。
	ctx := context.WithoutCancel(r.Context())
	rec := &recorder{ResponseWriter: w}
	completed := false
	defer func() {
		if completed {
			return
		}
		// 处理器 panic 或写出 5xx、401、429：删除记录，允许重试。
		_ = q.DeleteIdempotencyKey(ctx, id)
	}()
	next.ServeHTTP(rec, r)
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	if !cacheable(status) {
		return
	}
	hdr := map[string][]string{}
	for k, v := range rec.header {
		if !skipHeaders[k] {
			hdr[k] = v
		}
	}
	hj, err := json.Marshal(hdr)
	if err != nil {
		return
	}
	st := int32(status)
	if n, err := q.CompleteIdempotencyKey(ctx, sqlc.CompleteIdempotencyKeyParams{
		ID: id, ResponseStatus: &st, ResponseHeaders: hj, ResponseBody: rec.body.Bytes(),
	}); err != nil || n == 0 {
		// 响应已发出，无法再改为错误。写入失败时删除记录，让重试重新执行而不是一直返回 409；
		// 影响 0 行说明记录已被当作放弃的请求删除（超过 Abandoned），同样无需处理。
		return
	}
	completed = true
}

// cacheable 判断响应是否保存为幂等记录（CONV-12）。
func cacheable(status int) bool {
	return status < 500 && status != http.StatusUnauthorized && status != http.StatusTooManyRequests
}

func replay(w http.ResponseWriter, row sqlc.GetIdempotencyKeyRow) {
	var hdr map[string][]string
	_ = json.Unmarshal(row.ResponseHeaders, &hdr)
	for k, v := range hdr {
		w.Header()[k] = v
	}
	w.WriteHeader(int(*row.ResponseStatus))
	_, _ = w.Write(row.ResponseBody)
}

// recorder 在写出响应的同时保存状态码、响应头与响应体。
type recorder struct {
	http.ResponseWriter
	status int
	header http.Header
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
		r.header = r.ResponseWriter.Header().Clone()
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Sweep 删除过期记录，返回删除的行数（worker 每小时执行，CONV-12）。
func (s Store) Sweep(ctx context.Context) (int64, error) {
	return sqlc.New(s.Pool).DeleteExpiredIdempotencyKeys(ctx, s.Clock.Now())
}
