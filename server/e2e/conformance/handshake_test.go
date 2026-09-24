// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"bytes"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
	"time"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/nodewire"
	"github.com/akari-project/panel/server/e2e/testgateway"
)

// TestAgent_EnrollHandshakeOnline：接入（NODE-02）、握手（20.3）、能力上报（AGT-08）、
// 下发入站后进入 online（NODE-20）。
func TestAgent_EnrollHandshakeOnline(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, _ := e.start(startOpts{})

	n := e.node(id)
	first := n.Handshakes[0]
	if first.Reject != 0 || first.ConfigVersion != 0 {
		t.Fatalf("first handshake = %+v, want accepted with config_version 0 (new install)", first)
	}
	if first.ProtoVersion != nodewire.ProtoVersion {
		t.Errorf("proto_version = %d, want %d", first.ProtoVersion, nodewire.ProtoVersion)
	}
	caps := n.Capabilities
	if caps == nil || len(caps.GetKernels()) == 0 {
		t.Fatalf("Capabilities.kernels empty (AGT-08): %v", caps)
	}
	running := false
	for _, k := range caps.GetKernels() {
		if k.GetKernel() == nodev1.KernelType_KERNEL_TYPE_UNSPECIFIED || k.GetVersion() == "" {
			t.Errorf("kernel entry without type or version: %v", k)
		}
		if len(k.GetStableProtocols()) == 0 || len(k.GetStableTransports()) == 0 {
			t.Errorf("kernel %s reports no stable protocols or transports (AGT-08): %v", k.GetKernel(), k)
		}
		running = running || k.GetKernel() == caps.GetKernel()
	}
	if !running {
		t.Errorf("running kernel %s is not in Capabilities.kernels", caps.GetKernel())
	}
	if !caps.GetSupportsQuotaLease() {
		t.Log("note: agent does not declare supports_quota_lease")
	}

	// 已连接、没有入站：pending_config；入站就绪后 online。
	e.waitNode(id, "pending_config", func(n testgateway.NodeInfo) bool { return n.State == testgateway.StatePendingConfig && n.Statuses > 0 })
	e.configure(id)
}

// TestAgent_HelloRejectReasons：Agent 对 hello_reject 的每种原因按 NODE-22 的表处理，
// 任何原因都不会让 Agent 永久停止重连。
func TestAgent_HelloRejectReasons(t *testing.T) {
	t.Parallel()
	const busyRetry = 1500 // ms
	cases := []struct {
		reason     nodev1.HelloRejectReason
		retryAfter uint32
		expect     string // retry：按退避重试；immediate：立即以新 nonce 重试；after：按 retry_after_ms；hourly
	}{
		{nodev1.HelloRejectReason_HELLO_REJECT_REASON_CLOCK_SKEW, 0, "retry"},
		{nodev1.HelloRejectReason_HELLO_REJECT_REASON_AUTH_FAILED, 0, "retry"},
		{nodev1.HelloRejectReason_HELLO_REJECT_REASON_REPLAY, 0, "immediate"},
		{nodev1.HelloRejectReason_HELLO_REJECT_REASON_BUSY, busyRetry, "after"},
		{nodev1.HelloRejectReason_HELLO_REJECT_REASON_VERSION_UNSUPPORTED, 0, "hourly"},
		{nodev1.HelloRejectReason_HELLO_REJECT_REASON_REVOKED, 0, "hourly"},
		{nodev1.HelloRejectReason_HELLO_REJECT_REASON_SUPERSEDED, 0, "hourly"},
	}
	for _, c := range cases {
		t.Run(strings.ToLower(strings.TrimPrefix(c.reason.String(), "HELLO_REJECT_REASON_")), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, testgateway.Config{})
			id, _ := e.start(startOpts{})
			e.gw.InjectReject(id, c.reason, c.retryAfter, 1)
			e.gw.DropSession(id)
			n := e.waitNode(id, "injected rejection", func(n testgateway.NodeInfo) bool {
				return slices.ContainsFunc(n.Handshakes, func(h testgateway.Handshake) bool { return h.Injected })
			})
			idx := slices.IndexFunc(n.Handshakes, func(h testgateway.Handshake) bool { return h.Injected })
			rejected := n.Handshakes[idx]

			if c.expect == "hourly" {
				// NODE-22：每小时重试一次。探测窗口远小于 1 小时（按实现的退避缩放）。
				probe := min(3*time.Second, e.tm.Hour()/10)
				e.consistently(probe, id, "no retry before the hourly interval", func(n testgateway.NodeInfo) bool {
					return len(n.Handshakes) == idx+1
				})
				return
			}
			n = e.waitNode(id, "retry after rejection", func(n testgateway.NodeInfo) bool {
				return len(n.Handshakes) > idx+1 && n.Connected
			})
			next := n.Handshakes[idx+1]
			gap := next.At.Sub(rejected.At)
			if bytes.Equal(next.Nonce, rejected.Nonce) {
				t.Errorf("retry reused nonce %x (NODE-09)", next.Nonce)
			}
			switch c.expect {
			case "immediate":
				if gap > 2*time.Second {
					t.Errorf("replay: retried after %v, want immediately", gap)
				}
			case "after":
				if want := time.Duration(c.retryAfter)*time.Millisecond - 50*time.Millisecond; gap < want {
					t.Errorf("busy: retried after %v, want ≥ retry_after_ms %dms", gap, c.retryAfter)
				}
			case "retry":
				if gap > e.tm.FirstRetryBound() {
					t.Errorf("%s: retried after %v, want ≤ %v (NODE-07 backoff)", c.reason, gap, e.tm.FirstRetryBound())
				}
			}
		})
	}
}

// TestAgent_ClockSkewRetries：控制面时钟偏差超过 60 秒时握手被拒（NODE-08），Agent 持续按退避重试，
// 时钟恢复后重新上线。
func TestAgent_ClockSkewRetries(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, _ := e.start(startOpts{})
	gen := e.node(id).SessionGen
	e.clk.set(2 * time.Minute)
	e.gw.DropSession(id)
	skews := func(n testgateway.NodeInfo) int {
		c := 0
		for _, h := range n.Handshakes {
			if h.Reject == nodev1.HelloRejectReason_HELLO_REJECT_REASON_CLOCK_SKEW && !h.Injected {
				c++
			}
		}
		return c
	}
	e.waitNode(id, "two clock_skew rejections", func(n testgateway.NodeInfo) bool { return skews(n) >= 2 })
	e.clk.set(0)
	e.waitNode(id, "reconnect after clock recovers", func(n testgateway.NodeInfo) bool { return n.Connected && n.SessionGen > gen })
}

// TestAgent_FreshNonces：每次握手的 nonce 都是新的 16 字节随机数（NODE-09）。
func TestAgent_FreshNonces(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, _ := e.start(startOpts{})
	for range 5 {
		gen := e.node(id).SessionGen
		e.gw.DropSession(id)
		e.waitNode(id, "reconnect", func(n testgateway.NodeInfo) bool { return n.Connected && n.SessionGen > gen })
	}
	seen := map[string]bool{}
	for _, h := range e.node(id).Handshakes {
		if len(h.Nonce) != nodewire.HelloNonceSize {
			t.Fatalf("nonce length %d", len(h.Nonce))
		}
		k := hex.EncodeToString(h.Nonce)
		if seen[k] {
			t.Fatalf("nonce %s reused", k)
		}
		seen[k] = true
	}
}
