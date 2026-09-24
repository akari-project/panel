// SPDX-License-Identifier: AGPL-3.0-or-later

package fakeagent

import (
	"context"
	"sync"
	"time"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"
)

// EventKind 是模拟 Agent 记录的事件类型。
type EventKind string

// 事件类型。
const (
	EventDial          EventKind = "dial"           // 开始一次连接尝试
	EventConnected     EventKind = "connected"      // 握手成功
	EventRejected      EventKind = "rejected"       // 收到 hello_reject 或拒绝类关闭码
	EventDisconnected  EventKind = "disconnected"   // 会话或连接尝试结束
	EventApplied       EventKind = "applied"        // 应用了一条带版本的指令
	EventIgnored       EventKind = "ignored"        // 确认但不应用（版本不大于本地、校验失败等）
	EventFullRequested EventKind = "full_requested" // 断开并以 config_version=0 重连（NODE-23）
	EventApplyFailed   EventKind = "apply_failed"   // 指令应用失败，保留原配置（AGT-07）
	EventLease         EventKind = "lease"          // 收到 QuotaLease
	EventLeaseRequest  EventKind = "lease_request"  // 发送 LeaseRequest
	EventConnsClosed   EventKind = "conns_closed"   // 模拟关闭某账号的全部连接（租约耗尽或超时，ACC-08、ACC-09）
	EventReportAcked   EventKind = "report_acked"   // 流量报告被确认，移出 WAL
	EventUpgrade       EventKind = "upgrade"        // 收到 AgentUpgrade
	EventCredExpired   EventKind = "cred_expired"   // 凭据按 expires_at_ms 本地到期（NODE-24）
)

// Event 是一条事件记录。
type Event struct {
	Kind    EventKind
	At      time.Time
	Reason  nodev1.HelloRejectReason // EventRejected
	Delay   time.Duration            // 下一次连接尝试之前的等待（EventRejected、EventDisconnected）
	Body    string                   // 指令类型，如 "cred_upsert"
	Version uint64                   // 指令的配置版本
	Detail  string
	Account string
	Seq     uint64 // EventReportAcked 的 report_seq
	Mode    nodev1.SyncMode
}

const maxEvents = 20000

type eventLog struct {
	mu      sync.Mutex
	events  []Event
	dropped int
	changed chan struct{}
}

func newEventLog() *eventLog { return &eventLog{changed: make(chan struct{})} }

func (l *eventLog) add(e Event) {
	l.mu.Lock()
	if len(l.events) >= maxEvents {
		n := maxEvents / 10
		l.events = append(l.events[:0], l.events[n:]...)
		l.dropped += n
	}
	l.events = append(l.events, e)
	ch := l.changed
	l.changed = make(chan struct{})
	l.mu.Unlock()
	close(ch)
}

// snapshot 返回保留的事件、第一条事件的绝对序号（此前已裁剪的条数）与变更通知通道。
func (l *eventLog) snapshot() ([]Event, int, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.events...), l.dropped, l.changed
}

// Events 返回已记录的事件（最多保留最近 20,000 条）。
func (a *Agent) Events() []Event {
	ev, _, _ := a.events.snapshot()
	return ev
}

// WaitEvent 等待第一条满足 match 的事件（包括已经记录的），ctx 结束时返回 false。
func (a *Agent) WaitEvent(ctx context.Context, match func(Event) bool) (Event, bool) {
	seen := 0 // 已检查过的事件的绝对序号上界；裁剪不会让事件被跳过或重复检查
	for {
		ev, dropped, ch := a.events.snapshot()
		for i := max(seen-dropped, 0); i < len(ev); i++ {
			if match(ev[i]) {
				return ev[i], true
			}
		}
		seen = dropped + len(ev)
		select {
		case <-ch:
		case <-ctx.Done():
			return Event{}, false
		}
	}
}

// CountEvents 返回满足 match 的事件数。
func (a *Agent) CountEvents(match func(Event) bool) int {
	n := 0
	for _, e := range a.Events() {
		if match(e) {
			n++
		}
	}
	return n
}
