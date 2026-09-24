// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/auth"
)

// 登录失败冷却（AUTH-09）：同一账号 5 次失败后冷却 15 分钟；二次验证与重新验证的失败同样计入。
const (
	MaxFailures   = 5
	LoginCooldown = 15 * time.Minute
)

// reserve 在校验之前原子地检查冷却并预占一次尝试：冷却中拒绝；已有 5 次未释放的尝试时进入冷却并拒绝。
// 计数在校验之前增加，并发请求在冷却生效前也无法超过 5 次尝试。只有完整登录成功（或重新验证成功）
// 才清除计数，密码正确但尚未完成二次验证不清除，否则可以借此无限尝试二次验证码。
var reserve = valkey.NewLuaScript(`
local ttl = redis.call('PTTL', KEYS[1])
if ttl > 0 then return {0, ttl} end
local n = redis.call('INCR', KEYS[2])
if n == 1 then redis.call('PEXPIRE', KEYS[2], ARGV[2]) end
if n > tonumber(ARGV[1]) then
  redis.call('SET', KEYS[1], '1', 'PX', ARGV[2])
  redis.call('DEL', KEYS[2])
  return {0, tonumber(ARGV[2])}
end
return {1, 0}
`)

func cooldownKey(email string) string { return "login:cool:" + email }
func attemptsKey(email string) string { return "login:fail:" + email }

// reserveAttempt 执行登录限流：同一 IP 每分钟 20 次，以及按邮箱的失败冷却。冷却按邮箱计数，
// 对不存在的邮箱同样生效，返回相同的 429（AUTH-09）。
func (s *Service) reserveAttempt(ctx context.Context, ip, email string) error {
	ok, retry, err := s.Limiter.Allow(ctx, LoginPerIP, ip)
	if err != nil {
		return apierr.Unavailable(err)
	}
	if !ok {
		return apierr.RateLimited(retry)
	}
	kctx, cancel := context.WithTimeout(ctx, auth.Timeout)
	defer cancel()
	vals, err := reserve.Exec(kctx, s.KV, []string{cooldownKey(email), attemptsKey(email)},
		[]string{strconv.Itoa(MaxFailures), strconv.FormatInt(LoginCooldown.Milliseconds(), 10)}).ToArray()
	if err != nil {
		return apierr.Unavailable(err)
	}
	allowed, _ := vals[0].AsInt64()
	if allowed == 1 {
		return nil
	}
	ms, _ := vals[1].AsInt64()
	return apierr.RateLimited(time.Duration(ms) * time.Millisecond)
}

// clearAttempts 在登录或重新验证成功后清除失败计数。
func (s *Service) clearAttempts(ctx context.Context, email string) error {
	kctx, cancel := context.WithTimeout(ctx, auth.Timeout)
	defer cancel()
	if err := s.KV.Do(kctx, s.KV.B().Del().Key(attemptsKey(email)).Build()).Error(); err != nil {
		return apierr.Unavailable(err)
	}
	return nil
}
