// SPDX-License-Identifier: AGPL-3.0-or-later

// Package kv 建立到 Valkey 的连接（spec/41：Valkey 8 及以上，客户端 valkey-go）。
//
// Valkey 保存吊销集合、限流计数、短期状态与缓存；其中出现的敏感值按 CONV-19 加密。
// 依赖故障时接口返回 503 service_unavailable（spec/40 40.4），不回退为放行。
package kv

import (
	"context"
	"fmt"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Open 按 URL（valkey://、redis://、rediss://、unix://）连接并执行一次 PING。
func Open(ctx context.Context, url string) (valkey.Client, error) {
	opt, err := valkey.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("kv: %w", err)
	}
	c, err := valkey.NewClient(opt)
	if err != nil {
		return nil, fmt.Errorf("kv: connect: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Do(pctx, c.B().Ping().Build()).Error(); err != nil {
		c.Close()
		return nil, fmt.Errorf("kv: ping: %w", err)
	}
	return c, nil
}
