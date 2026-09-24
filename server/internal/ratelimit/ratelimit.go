// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ratelimit 是基于 Valkey 的固定窗口限流（spec/30 API-04、spec/10 AUTH-09）。
//
// 每个键在窗口开始时建立计数，窗口结束时由 Valkey 过期；多实例共享同一计数。
package ratelimit

import (
	"context"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Rule 是一条限流规则：每个 Window 内最多 Limit 次。
type Rule struct {
	Name   string
	Limit  int64
	Window time.Duration
}

// Limiter 执行限流。
type Limiter struct {
	KV valkey.Client
}

// 计数加一；首次计数时设置过期。返回计数与剩余毫秒。
var incr = valkey.NewLuaScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then redis.call('PEXPIRE', KEYS[1], ARGV[1]); ttl = tonumber(ARGV[1]) end
return {n, ttl}
`)

func key(r Rule, subject string) string { return "rl:" + r.Name + ":" + subject }

// Timeout 是单次 Valkey 调用的时限（spec/40 40.4）。
const Timeout = 2 * time.Second

// Allow 为 subject 计数一次。超出上限时 ok 为 false，retry 为窗口剩余时间。
func (l Limiter) Allow(ctx context.Context, r Rule, subject string) (ok bool, retry time.Duration, err error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	vals, err := incr.Exec(ctx, l.KV, []string{key(r, subject)}, []string{strconv.FormatInt(r.Window.Milliseconds(), 10)}).ToArray()
	if err != nil {
		return false, 0, err
	}
	n, err := vals[0].AsInt64()
	if err != nil {
		return false, 0, err
	}
	ttl, err := vals[1].AsInt64()
	if err != nil {
		return false, 0, err
	}
	if n > r.Limit {
		return false, time.Duration(ttl) * time.Millisecond, nil
	}
	return true, 0, nil
}

// Count 返回 subject 在当前窗口的计数与剩余时间，不计数（用于冷却检查，AUTH-09）。
func (l Limiter) Count(ctx context.Context, r Rule, subject string) (int64, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	k := key(r, subject)
	res := l.KV.DoMulti(ctx, l.KV.B().Get().Key(k).Build(), l.KV.B().Pttl().Key(k).Build())
	n, err := res[0].AsInt64()
	if valkey.IsValkeyNil(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	ttl, err := res[1].AsInt64()
	if err != nil {
		return 0, 0, err
	}
	return n, time.Duration(max(ttl, 0)) * time.Millisecond, nil
}

// Reset 清除 subject 的计数（例如登录成功后清除失败计数）。
func (l Limiter) Reset(ctx context.Context, r Rule, subject string) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	return l.KV.Do(ctx, l.KV.B().Del().Key(key(r, subject)).Build()).Error()
}
