// SPDX-License-Identifier: AGPL-3.0-or-later

package nodewire

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/internal/clock"
)

// chanConn 是内存中的 Conn，用于不经网络测试会话引擎。
type chanConn struct {
	in, out chan []byte
	closed  chan struct{}
	once    *sync.Once
}

func newConnPair() (*chanConn, *chanConn) {
	ab, ba := make(chan []byte, 64), make(chan []byte, 64)
	closed := make(chan struct{}) // 同一条连接：任一端关闭，两端都看到
	once := new(sync.Once)
	return &chanConn{in: ba, out: ab, closed: closed, once: once}, &chanConn{in: ab, out: ba, closed: closed, once: once}
}

func (c *chanConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case b := <-c.in:
		return b, nil
	case <-c.closed:
		return nil, errors.New("closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *chanConn) Write(ctx context.Context, b []byte) error {
	select {
	case c.out <- b:
		return nil
	case <-c.closed:
		return errors.New("closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *chanConn) Close(int, string) error { return c.Abort() }

func (c *chanConn) Abort() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *chanConn) Ping(context.Context) error { return nil }

func sessionPair(t *testing.T, faultsA FaultPlan, boundedA bool, windowA int) (a, b *Session) {
	t.Helper()
	ca, cb := newConnPair()
	k1, k2 := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(k1)
	_, _ = rand.Read(k2)
	a, err := NewSession(SessionConfig{Conn: ca, SendKey: k1, RecvKey: k2, SendDir: DirPanelToAgent, Clock: clock.Real{}, Faults: faultsA, Bounded: boundedA, WindowMessages: windowA})
	if err != nil {
		t.Fatal(err)
	}
	b, err = NewSession(SessionConfig{Conn: cb, SendKey: k2, RecvKey: k1, SendDir: DirAgentToPanel, Clock: clock.Real{}})
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func statusOut(idem string) *Outgoing {
	return NewOutgoing(&nodev1.Envelope{IdemKey: idem, Body: &nodev1.Envelope_ReportStatus{ReportStatus: &nodev1.ReportStatus{}}}, nil)
}

func waitDone(t *testing.T, s *Session, what string) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-s.Done():
	case <-timer.C:
		t.Fatalf("%s: session not done (err=%v)", what, s.Err())
	}
}

// TestSessionInboundDisconnectEnds：入方向 Disconnect 故障与 Abort 之后，两个协程都退出、done 关闭。
// 回归：Abort 曾不取消 ctx，写协程一直等待，会话永不结束。
func TestSessionInboundDisconnectEnds(t *testing.T) {
	a, b := sessionPair(t, FaultFunc(func(d Direction, _ uint64, _ *nodev1.Envelope) Action {
		return Action{Disconnect: d == Inbound}
	}), false, 0)
	a.Start(context.Background())
	b.Start(context.Background())
	_ = b.Send(statusOut("x"))
	waitDone(t, a, "a")
	waitDone(t, b, "b")
	if err := a.Send(statusOut("y")); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Send after abort = %v", err)
	}
}

// TestSessionAbortBeforeStart：登记后、启动前被中断的会话，启动后立即结束。
func TestSessionAbortBeforeStart(t *testing.T) {
	a, _ := sessionPair(t, nil, false, 0)
	a.Abort()
	a.Start(context.Background())
	waitDone(t, a, "a")
}

// TestSessionKeyCoalesce：未发送的同 Key 条目只保留最新一条。
func TestSessionKeyCoalesce(t *testing.T) {
	a, _ := sessionPair(t, nil, false, 0)
	for i := range 10 {
		o := statusOut(string(rune('a' + i)))
		o.Key = "status"
		if err := a.Send(o); err != nil {
			t.Fatal(err)
		}
	}
	if u := a.Unacked(); len(u) != 1 || u[0].Env.GetIdemKey() != "j" {
		t.Fatalf("unacked = %d items, want only the latest", len(u))
	}
}

// TestSessionExemptFull：Reset 放入的豁免条目不计入有界窗口，之后的条目照常排队。
func TestSessionExemptFull(t *testing.T) {
	a, _ := sessionPair(t, nil, true, 2)
	_ = a.Send(statusOut("1"))
	_ = a.Send(statusOut("2"))
	if err := a.Send(statusOut("3")); !errors.Is(err, ErrWindowFull) {
		t.Fatalf("third send = %v, want ErrWindowFull", err)
	}
	full := statusOut("full")
	full.Exempt = true
	if err := a.Reset(full); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"4", "5"} {
		if err := a.Send(statusOut(k)); err != nil {
			t.Fatalf("send %s after reset: %v", k, err)
		}
	}
	if err := a.Send(statusOut("6")); !errors.Is(err, ErrWindowFull) {
		t.Fatalf("send beyond window = %v", err)
	}
}

// TestSessionAckBeyondSent：确认从未发送的 seq 按 NODE-12 视为协议错误。
func TestSessionAckBeyondSent(t *testing.T) {
	a, b := sessionPair(t, nil, false, 0)
	a.Start(context.Background())
	// b 不启动，直接构造一个 ack=5 的帧发给 a。
	env := &nodev1.Envelope{Ack: 5, IdemKey: "z", Body: &nodev1.Envelope_ReportStatus{ReportStatus: &nodev1.ReportStatus{}}}
	frame, err := b.seal(1, env)
	if err != nil {
		t.Fatal(err)
	}
	b.cfg.Conn.(*chanConn).out <- frame
	waitDone(t, a, "a")
	if err := a.Err(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want protocol violation", err)
	}
}
