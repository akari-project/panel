// SPDX-License-Identifier: AGPL-3.0-or-later

package nodewire

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"
)

// Direction 区分故障作用的方向。
type Direction int

// 故障方向。
const (
	Outbound Direction = iota + 1 // 本端发出的帧
	Inbound                       // 本端收到的帧
)

func (d Direction) String() string {
	if d == Outbound {
		return "outbound"
	}
	return "inbound"
}

// Action 是对一个会话帧注入的故障。零值表示正常处理。
//
// WebSocket 基于有序可靠的 TCP，同一连接内的“丢包”只能表现为序号跳跃：接收方发现跳号后关闭连接，
// 由重连后的重传补齐（NODE-12）。因此 Drop 与 Duplicate 都会导致连接关闭，Resend 不会。
type Action struct {
	Drop       bool          // 丢弃本帧
	Duplicate  bool          // 以同一 seq 重复发送本帧（仅出方向），接收方必须关闭连接
	Resend     bool          // 以新的 seq 再发一次同一信封，idem_key 不变（仅出方向），接收方必须去重
	Delay      time.Duration // 处理本帧之前等待
	Disconnect bool          // 处理本帧之前直接断开底层连接（不发送关闭帧）
}

// FaultPlan 为会话中的每个帧决定注入的故障。n 为该方向在本会话内的帧序号（即 seq），
// env 为明文信封（入方向在解密之后、应用之前判定）。实现必须并发安全。
type FaultPlan interface {
	Decide(dir Direction, n uint64, env *nodev1.Envelope) Action
}

// FaultFunc 把函数适配为 FaultPlan。
type FaultFunc func(dir Direction, n uint64, env *nodev1.Envelope) Action

// Decide 实现 FaultPlan。
func (f FaultFunc) Decide(dir Direction, n uint64, env *nodev1.Envelope) Action {
	return f(dir, n, env)
}

// RandomFaults 按概率注入故障，概率取值 [0, 1]，由 Seed 决定可复现的随机序列。
type RandomFaults struct {
	Seed       uint64
	Directions []Direction // 为空表示两个方向
	Drop       float64
	Duplicate  float64
	Resend     float64
	Disconnect float64
	Delay      time.Duration // 固定延迟
	Jitter     time.Duration // 额外的随机延迟上限

	once sync.Once
	mu   sync.Mutex
	rng  *rand.Rand
}

// Decide 实现 FaultPlan。
func (r *RandomFaults) Decide(dir Direction, _ uint64, _ *nodev1.Envelope) Action {
	if len(r.Directions) > 0 {
		ok := false
		for _, d := range r.Directions {
			ok = ok || d == dir
		}
		if !ok {
			return Action{}
		}
	}
	r.once.Do(func() { r.rng = rand.New(rand.NewPCG(r.Seed, r.Seed^0x9e3779b97f4a7c15)) })
	r.mu.Lock()
	defer r.mu.Unlock()
	var a Action
	a.Disconnect = r.rng.Float64() < r.Disconnect
	a.Drop = r.rng.Float64() < r.Drop
	if dir == Outbound {
		a.Duplicate = r.rng.Float64() < r.Duplicate
		a.Resend = r.rng.Float64() < r.Resend
	}
	a.Delay = r.Delay
	if r.Jitter > 0 {
		a.Delay += time.Duration(r.rng.Int64N(int64(r.Jitter)))
	}
	return a
}

// FaultSwitch 是可以在运行中替换的 FaultPlan；零值表示不注入故障。
type FaultSwitch struct {
	p atomic.Pointer[FaultPlan]
}

// Set 替换当前的故障计划；nil 表示停止注入。
func (s *FaultSwitch) Set(p FaultPlan) {
	if p == nil {
		s.p.Store(nil)
		return
	}
	s.p.Store(&p)
}

// Decide 实现 FaultPlan。
func (s *FaultSwitch) Decide(dir Direction, n uint64, env *nodev1.Envelope) Action {
	if p := s.p.Load(); p != nil {
		return (*p).Decide(dir, n, env)
	}
	return Action{}
}
