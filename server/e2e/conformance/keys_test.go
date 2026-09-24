// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"context"
	"slices"
	"testing"
	"time"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/fakeagent"
	"github.com/akari-project/panel/server/e2e/testgateway"
)

// TestAgent_ReEnrollTransition：重新签发接入令牌后，新装的 Agent 接入并握手成功（NODE-03）：
// 旧 PSK 的会话被取代（NODE-21），psk_prev 清空，节点保留原有配置并收敛。
func TestAgent_ReEnrollTransition(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, old := e.start(startOpts{})
	v := e.configure(id)
	oldGen := e.node(id).SessionGen

	fresh := e.startWithToken(id, nil)
	n := e.waitNode(id, "new PSK handshake clears psk_prev", func(n testgateway.NodeInfo) bool {
		return n.Connected && n.SessionGen > oldGen && !n.HasPrevPSK
	})
	h := n.Handshakes[lastAccepted(n)]
	if h.PrevPSK {
		t.Fatalf("latest session was authenticated with the previous PSK")
	}
	if h.ConfigVersion != 0 {
		t.Errorf("re-installed agent sent config_version %d, want 0", h.ConfigVersion)
	}
	n = e.converged(id)
	if n.Version != v {
		t.Fatalf("configuration not preserved across re-enrollment: version %d, want %d", n.Version, v)
	}
	if st, ok := inspect(fresh); ok && !slices.Contains(st.Credentials, "cred-base") {
		t.Errorf("re-enrolled agent lacks the preserved credential: %v", st.Credentials)
	}
	// 旧实例的会话已被关闭，此后不能再以旧 PSK 建立会话。
	gen := n.SessionGen
	e.consistently(3*time.Second, id, "old agent does not take the node back", func(n testgateway.NodeInfo) bool {
		return n.SessionGen == gen && n.Connected
	})
	old.Stop()
}

// TestAgent_KeyRevocation：立即吊销后会话以 4006 关闭（NODE-19），Agent 不再频繁重连；
// 签发新令牌重新接入后恢复。
func TestAgent_KeyRevocation(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, inst := e.start(startOpts{})
	e.configure(id)
	e.gw.RevokeKey(id)
	n := e.waitNode(id, "session closed", func(n testgateway.NodeInfo) bool { return !n.Connected })
	if n.State != testgateway.StatePendingEnroll {
		t.Fatalf("state after revocation = %s, want pending_enroll", n.State)
	}
	attempts := len(n.Handshakes)
	// revoked：每小时重试一次。在探测窗口内至多一次尝试，且都被拒绝。
	probe := min(3*time.Second, e.tm.Hour()/10)
	e.consistently(probe, id, "revoked agent backs off", func(n testgateway.NodeInfo) bool {
		for _, h := range n.Handshakes[attempts:] {
			if h.Reject != nodev1.HelloRejectReason_HELLO_REJECT_REASON_REVOKED {
				return false
			}
		}
		return len(n.Handshakes) <= attempts+1 && !n.Connected
	})
	inst.Stop()

	// 重新接入必须签发新的接入令牌。
	e.startWithToken(id, nil)
	e.converged(id)
}

// TestAgent_RevokedCloseCode：已建立的会话被吊销时，Agent 看到的是拒绝原因 revoked（关闭码 4006）。
// 这一条只检查模拟 Agent 的内部判定；真实 Agent 的同一行为由 TestAgent_KeyRevocation 从外部观察。
func TestAgent_RevokedCloseCode(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, inst := e.start(startOpts{})
	fi, ok := inst.(*FakeInstance)
	if !ok {
		t.Skip("white-box check for the fake agent")
	}
	e.gw.RevokeKey(id)
	ctx, cancel := context.WithTimeout(t.Context(), e.tm.Patience)
	defer cancel()
	ev, ok := fi.Agent().WaitEvent(ctx, func(ev fakeagent.Event) bool { return ev.Kind == fakeagent.EventRejected })
	if !ok || ev.Reason != nodev1.HelloRejectReason_HELLO_REJECT_REASON_REVOKED {
		t.Fatalf("event = %+v, want rejected(revoked)", ev)
	}
	if ev.Delay < e.tm.Hour()/2 {
		t.Fatalf("revoked retry delay %v, want about an hour (scaled %v)", ev.Delay, e.tm.Hour())
	}
}
