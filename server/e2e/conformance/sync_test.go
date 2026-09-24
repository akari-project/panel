// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"fmt"
	"slices"
	"testing"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/testgateway"
)

// TestAgent_ReconnectSync：离线期间只有凭据变化时，重连后按增量同步；中间含非凭据变更时发全量（NODE-15）。
// Agent 在两种情况下都收敛到控制面的当前版本，并以本地状态重启（AGT-05）。
func TestAgent_ReconnectSync(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, inst := e.start(startOpts{})
	e.configure(id)

	inst.Stop()
	e.waitNode(id, "disconnected", func(n testgateway.NodeInfo) bool { return !n.Connected })
	e.gw.UpsertCredentials(id, cred("cred-d1", "acct"))
	e.gw.UpsertCredentials(id, cred("cred-d2", "acct"))
	e.gw.RemoveCredentials(id, "cred-d1")
	gen := e.node(id).SessionGen
	inst.Restart(t)
	n := e.waitNode(id, "reconnect", func(n testgateway.NodeInfo) bool { return n.Connected && n.SessionGen > gen })
	if s := lastSync(n); s.Mode != nodev1.SyncMode_SYNC_MODE_DELTA || s.From == 0 {
		t.Errorf("credential-only changes: sync = %+v, want delta from the persisted version", s)
	}
	n = e.converged(id)
	if st, ok := inspect(inst); ok && !slices.Equal(st.Credentials, n.Credentials) {
		t.Errorf("after delta: agent %v, panel %v", st.Credentials, n.Credentials)
	}

	inst.Stop()
	e.waitNode(id, "disconnected", func(n testgateway.NodeInfo) bool { return !n.Connected })
	e.gw.UpsertCredentials(id, cred("cred-f1", "acct"))
	e.gw.SetInbounds(id, nodev1.KernelType_KERNEL_TYPE_UNSPECIFIED, vlessTCP("vless-tcp"), vlessTCP("vless-tcp-2"))
	e.gw.UpsertCredentials(id, cred("cred-f2", "acct"))
	gen = e.node(id).SessionGen
	inst.Restart(t)
	n = e.waitNode(id, "reconnect", func(n testgateway.NodeInfo) bool { return n.Connected && n.SessionGen > gen })
	if s := lastSync(n); s.Mode != nodev1.SyncMode_SYNC_MODE_FULL {
		t.Errorf("changes including inbounds: sync = %+v, want full", s)
	}
	n = e.converged(id)
	if got := len(n.LastStatus.GetInbounds()); got != 2 {
		t.Errorf("inbound health entries = %d, want 2", got)
	}
	if st, ok := inspect(inst); ok && !slices.Equal(st.Credentials, n.Credentials) {
		t.Errorf("after full: agent %v, panel %v", st.Credentials, n.Credentials)
	}
}

// TestAgent_WindowOverflowFull：控制面 → 节点的待确认条目超过窗口时，控制面丢弃积压改发全量（NODE-14），
// Agent 应用全量后收敛；有连接的账号重新申请租约（ACC-08）。
func TestAgent_WindowOverflowFull(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{WindowMessages: 8})
	id, inst := e.start(startOpts{})
	e.configure(id)
	cs, _ := inst.(ConnectionSource)
	leaseReqs := 0
	if cs != nil {
		e.gw.UpsertCredentials(id, cred("cred-lease", "acct-lease"))
		cs.OpenConnection("acct-lease")
		n := e.waitNode(id, "initial lease request", func(n testgateway.NodeInfo) bool { return len(n.LeaseRequests) > 0 })
		leaseReqs = len(n.LeaseRequests)
	}
	e.gw.PauseReading(id, true)
	for i := range 20 {
		e.gw.UpsertCredentials(id, cred(fmt.Sprintf("cred-w%02d", i), "acct"))
	}
	e.gw.PauseReading(id, false)
	n := e.converged(id)
	if n.Overflows == 0 {
		t.Fatalf("no window overflow recorded (window 8, 20 unacknowledged commands)")
	}
	if st, ok := inspect(inst); ok && !slices.Equal(st.Credentials, n.Credentials) {
		t.Errorf("agent %v, panel %v", st.Credentials, n.Credentials)
	}
	if cs != nil {
		e.waitNode(id, "lease re-requested after full sync", func(n testgateway.NodeInfo) bool { return len(n.LeaseRequests) > leaseReqs })
	}
}
