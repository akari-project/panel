// SPDX-License-Identifier: AGPL-3.0-or-later

// Package clock 提供可注入的时钟。业务代码只能通过 Clock 取得时间（CONV-04），
// 本包是仓库中唯一允许调用 time.Now 的地方（make lint 中的 check-clock 检查）。
package clock

import (
	"sync"
	"time"
)

// Clock 是业务代码取得当前时间的唯一途径。
type Clock interface {
	Now() time.Time
}

// Real 返回系统时间。
type Real struct{}

// Now 返回当前系统时间。
func (Real) Now() time.Time { return time.Now() }

// Fake 是测试用的手动时钟，并发安全。零值不可用，用 NewFake 创建。
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake 返回停在 t 的时钟。
func NewFake(t time.Time) *Fake { return &Fake{now: t} }

// Now 返回当前设定的时间。
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Set 把时钟设为 t。
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}

// Advance 把时钟向前拨 d，返回拨动后的时间。
func (f *Fake) Advance(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	return f.now
}
