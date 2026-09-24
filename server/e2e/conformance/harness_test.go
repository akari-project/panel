// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/testgateway"
	"github.com/akari-project/panel/server/internal/clock"
)

// env 是一个用例的环境：一个测试用控制面端与被测实现。
type env struct {
	t    *testing.T
	gw   *testgateway.Server
	subj Subject
	tm   Timing
	clk  *offsetClock
}

// offsetClock 是带可调偏移的真实时钟，用于制造 Agent 与控制面之间的时钟偏差（NODE-08）。
type offsetClock struct{ off atomic.Int64 }

func (c *offsetClock) Now() time.Time      { return clock.Real{}.Now().Add(time.Duration(c.off.Load())) }
func (c *offsetClock) set(d time.Duration) { c.off.Store(int64(d)) }

func newEnv(t *testing.T, cfg testgateway.Config) *env {
	t.Helper()
	subj := SelectSubject(t)
	clk := &offsetClock{}
	if cfg.Clock == nil {
		cfg.Clock = clk
	}
	return &env{t: t, gw: testgateway.Start(t, cfg), subj: subj, tm: subj.Timing(), clk: clk}
}

type startOpts struct {
	node testgateway.NodeOptions
	caps *nodev1.Capabilities
}

// start 建档、签发接入令牌、启动 Agent，并等待首次握手成功。
func (e *env) start(o startOpts) (string, Instance) {
	e.t.Helper()
	id := e.gw.CreateNode(o.node)
	inst := e.startWithToken(id, o.caps)
	return id, inst
}

// startWithToken 为已建档的节点签发新令牌并启动一个新装的 Agent（重新接入时即 NODE-03）。
func (e *env) startWithToken(id string, caps *nodev1.Capabilities) Instance {
	e.t.Helper()
	gen := e.node(id).SessionGen
	tok := e.gw.IssueEnrollToken(id)
	inst := e.subj.Start(e.t, StartOptions{
		GatewayURL: e.gw.URL, EnrollToken: tok, Client: e.gw.Client, CAFile: e.gw.CAFile, Capabilities: caps,
	})
	e.dumpOnFailure(inst)
	e.waitNode(id, "handshake after enrollment", func(n testgateway.NodeInfo) bool { return n.Connected && n.SessionGen > gen })
	return inst
}

// dumpOnFailure 在用例失败时输出模拟 Agent 最近的事件，便于定位。
func (e *env) dumpOnFailure(inst Instance) {
	fi, ok := inst.(*FakeInstance)
	if !ok {
		return
	}
	e.t.Cleanup(func() {
		if !e.t.Failed() {
			return
		}
		ev := fi.Agent().Events()
		if len(ev) > 40 {
			ev = ev[len(ev)-40:]
		}
		for _, x := range ev {
			e.t.Logf("agent event %s %s body=%s v=%d reason=%s delay=%v %s", x.At.Format("15:04:05.000"), x.Kind, x.Body, x.Version, x.Reason, x.Delay, x.Detail)
		}
	})
}

func (e *env) node(id string) testgateway.NodeInfo {
	n, ok := e.gw.Node(id)
	if !ok {
		e.t.Fatalf("unknown node %s", id)
	}
	return n
}

// waitNode 等待节点状态满足 cond，超时则失败。
func (e *env) waitNode(id, what string, cond func(testgateway.NodeInfo) bool) testgateway.NodeInfo {
	e.t.Helper()
	return e.waitNodeFor(e.tm.Patience, id, what, cond)
}

func (e *env) waitNodeFor(d time.Duration, id, what string, cond func(testgateway.NodeInfo) bool) testgateway.NodeInfo {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var last testgateway.NodeInfo
	if !e.gw.WaitNode(ctx, id, func(n testgateway.NodeInfo) bool { last = n; return cond(n) }) {
		e.t.Fatalf("timeout after %v waiting for %s; node: %s", d, what, describe(last))
	}
	return last
}

// consistently 在 d 时间内持续检查 cond，一旦不成立即失败。
func (e *env) consistently(d time.Duration, id, what string, cond func(testgateway.NodeInfo) bool) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var bad testgateway.NodeInfo
	if e.gw.WaitNode(ctx, id, func(n testgateway.NodeInfo) bool { bad = n; return !cond(n) }) {
		e.t.Fatalf("%s violated within %v; node: %s", what, d, describe(bad))
	}
}

// converged 等待节点在线，且当前会话上报的已应用版本等于控制面当前的配置版本。
func (e *env) converged(id string) testgateway.NodeInfo {
	e.t.Helper()
	return e.waitNode(id, "convergence", isConverged)
}

func isConverged(n testgateway.NodeInfo) bool {
	return n.Connected && n.StatusGen == n.SessionGen && n.AppliedVersion == n.Version
}

// configure 下发一个入站与一条凭据，等待节点 online（NODE-20）。
func (e *env) configure(id string) uint64 {
	e.t.Helper()
	e.gw.SetInbounds(id, nodev1.KernelType_KERNEL_TYPE_UNSPECIFIED, vlessTCP("vless-tcp"))
	v := e.gw.UpsertCredentials(id, cred("cred-base", "acct-base"))
	e.waitNode(id, "online", func(n testgateway.NodeInfo) bool {
		return n.State == testgateway.StateOnline && n.AppliedVersion == v
	})
	return v
}

func vlessTCP(tag string) *nodev1.Inbound {
	return &nodev1.Inbound{
		Tag: tag, Protocol: nodev1.Protocol_PROTOCOL_VLESS, Transport: nodev1.Transport_TRANSPORT_TCP, ListenPort: 443,
		SettingsJson: []byte(`{"transport":"tcp"}`),
	}
}

// cred 生成一条长度合法的凭据（messages.proto Credential）。
func cred(id, account string) *nodev1.Credential {
	c := &nodev1.Credential{Id: id, AccountId: account, Secret: random(16), Ss2022Key_16: random(16), Ss2022Key_32: random(32)}
	c.Secret[6] = 0x40 | c.Secret[6]&0x0f // UUIDv4
	c.Secret[8] = 0x80 | c.Secret[8]&0x3f
	return c
}

func random(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func describe(n testgateway.NodeInfo) string {
	var hs []string
	for _, h := range n.Handshakes {
		r := "ok"
		if h.Reject != 0 {
			r = strings.TrimPrefix(h.Reject.String(), "HELLO_REJECT_REASON_")
		}
		hs = append(hs, fmt.Sprintf("%s@%s(cv=%d)", r, h.At.Format("15:04:05.000"), h.ConfigVersion))
	}
	if len(hs) > 12 {
		hs = append(hs[:4], append([]string{"…"}, hs[len(hs)-8:]...)...)
	}
	return fmt.Sprintf("{state=%s version=%d applied=%d connected=%v gen=%d handshakes=[%s] overflows=%d}",
		n.State, n.Version, n.AppliedVersion, n.Connected, n.SessionGen, strings.Join(hs, " "), n.Overflows)
}

func trafficSource(t *testing.T, inst Instance) TrafficSource {
	t.Helper()
	ts, ok := inst.(TrafficSource)
	if !ok {
		t.Skip("subject does not implement TrafficSource")
	}
	return ts
}

func connectionSource(t *testing.T, inst Instance) ConnectionSource {
	t.Helper()
	cs, ok := inst.(ConnectionSource)
	if !ok {
		t.Skip("subject does not implement ConnectionSource")
	}
	return cs
}

// inspect 在被测实现提供 StateInspector 时返回已应用状态。
func inspect(inst Instance) (AppliedState, bool) {
	si, ok := inst.(StateInspector)
	if !ok {
		return AppliedState{}, false
	}
	return si.Applied(), true
}

// lastAccepted 返回最近一次成功握手的下标。
func lastAccepted(n testgateway.NodeInfo) int {
	for i := len(n.Handshakes) - 1; i >= 0; i-- {
		if n.Handshakes[i].Reject == 0 {
			return i
		}
	}
	return -1
}
