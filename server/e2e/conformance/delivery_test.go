// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/nodewire"
	"github.com/akari-project/panel/server/e2e/testgateway"
)

// TestAgent_TwoAgentsSameNode：同一节点的第二个实例握手成功后，旧会话以 superseded 关闭（NODE-21），
// 旧实例按 NODE-22 改为每小时重试，两者不会来回抢占。
func TestAgent_TwoAgentsSameNode(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, first := e.start(startOpts{})
	e.configure(id)
	gen := e.node(id).SessionGen
	first.Clone(t)
	n := e.waitNode(id, "second session", func(n testgateway.NodeInfo) bool { return n.Connected && n.SessionGen == gen+1 })
	attempts := len(n.Handshakes)
	probe := min(3*time.Second, e.tm.Hour()/10)
	e.consistently(probe, id, "no flapping between duplicate agents", func(n testgateway.NodeInfo) bool {
		return n.Connected && n.SessionGen == gen+1 && len(n.Handshakes) == attempts
	})
	e.converged(id)
}

// TestAgent_Delivery100Disconnects：断线 100 次后指令效果不丢不重（20.6）：
// 控制面在断线前、断线中、断线后下发凭据变更，Agent 最终应用到控制面的当前版本，
// 每个版本至多应用一次；同期的流量报告不丢、不重复入账（NODE-12、NODE-13、ACC-03）。
func TestAgent_Delivery100Disconnects(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, inst := e.start(startOpts{})
	e.configure(id)
	ts, _ := inst.(TrafficSource)

	var sentUp, sentDown uint64
	for i := range 100 {
		gen := e.node(id).SessionGen
		credID := fmt.Sprintf("cred-%03d", i)
		switch i % 4 {
		case 0: // 下发后立即断线：指令可能在途
			e.gw.UpsertCredentials(id, cred(credID, "acct-loop"))
			e.gw.DropSession(id)
		case 1: // 断线后下发：重连时按版本同步（NODE-15）
			e.gw.DropSession(id)
			e.gw.UpsertCredentials(id, cred(credID, "acct-loop"))
		case 2: // 移除一条较早的凭据后断线
			e.gw.RemoveCredentials(id, fmt.Sprintf("cred-%03d", i-2))
			e.gw.DropSession(id)
		case 3: // 已应用后再断线
			v := e.gw.UpsertCredentials(id, cred(credID, "acct-loop"))
			e.waitNode(id, "applied before drop", func(n testgateway.NodeInfo) bool { return n.AppliedVersion >= v })
			e.gw.DropSession(id)
		}
		if ts != nil {
			up, down := uint64(1000+i), uint64(2000+i)
			ts.AddTraffic("cred-base", up, down)
			ts.FlushTraffic()
			sentUp, sentDown = sentUp+up, sentDown+down
		}
		e.waitNode(id, fmt.Sprintf("reconnect %d", i+1), func(n testgateway.NodeInfo) bool { return n.Connected && n.SessionGen > gen })
	}
	n := e.converged(id)
	if got := n.SessionGen; got < 101 {
		t.Fatalf("only %d sessions, want ≥ 101 (100 disconnects)", got)
	}
	if st, ok := inspect(inst); ok {
		if !slices.Equal(st.Credentials, n.Credentials) {
			t.Errorf("agent credentials differ from control plane:\n agent %v\n panel %v", st.Credentials, n.Credentials)
		}
		checkVersionsOnce(t, st.Versions)
	}
	if ts != nil {
		ts.FlushTraffic()
		n = e.waitNode(id, "all traffic ingested", func(n testgateway.NodeInfo) bool {
			return n.Totals["cred-base"] == testgateway.Totals{Up: sentUp, Down: sentDown}
		})
		checkSeqsIncreasing(t, n.ReportSeqs)
		t.Logf("reports ingested %d, duplicates acknowledged without ingest %d", len(n.ReportSeqs), n.DupReports)
	}
}

// TestAgent_DeliveryUnderFrameFaults：控制面一侧随机丢帧、重复帧、以新 seq 重发同一信封与断线时，
// Agent 按 NODE-12 关闭连接并重传，按 idem_key 去重（NODE-13），最终收敛且不重复应用。
func TestAgent_DeliveryUnderFrameFaults(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, inst := e.start(startOpts{})
	e.configure(id)
	ts, _ := inst.(TrafficSource)
	e.gw.SetFaults(id, &nodewire.RandomFaults{Seed: 7, Drop: 0.03, Duplicate: 0.02, Resend: 0.1, Disconnect: 0.02, Jitter: 2 * time.Millisecond})
	var sentUp uint64
	for i := range 60 {
		e.gw.UpsertCredentials(id, cred(fmt.Sprintf("cred-f%02d", i), "acct-faults"))
		if i%5 == 4 {
			e.gw.RemoveCredentials(id, fmt.Sprintf("cred-f%02d", i-3))
		}
		if ts != nil {
			ts.AddTraffic("cred-base", 10, 0)
			ts.FlushTraffic()
			sentUp += 10
		}
	}
	// 故障期间可能反复断线；停止注入后必须收敛。
	e.waitNode(id, "some disconnects under faults", func(n testgateway.NodeInfo) bool { return n.SessionGen >= 3 || n.AppliedVersion == n.Version })
	e.gw.SetFaults(id, nil)
	gen := e.node(id).SessionGen
	e.gw.DropSession(id)
	e.waitNode(id, "reconnect", func(n testgateway.NodeInfo) bool { return n.Connected && n.SessionGen > gen })
	n := e.converged(id)
	if st, ok := inspect(inst); ok {
		if !slices.Equal(st.Credentials, n.Credentials) {
			t.Errorf("agent credentials differ from control plane:\n agent %v\n panel %v", st.Credentials, n.Credentials)
		}
		checkVersionsOnce(t, st.Versions)
	}
	if ts != nil {
		e.waitNode(id, "traffic exactly once", func(n testgateway.NodeInfo) bool { return n.Totals["cred-base"].Up == sentUp })
	}
}

// TestAgent_StaleCommandsIgnored：旧版本指令晚于全量快照到达时不被应用（20.6、NODE-23）：
// 版本不大于本地的增量只确认，不应用，也不触发全量同步。
func TestAgent_StaleCommandsIgnored(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, inst := e.start(startOpts{})
	e.configure(id)
	v := e.gw.UpsertCredentials(id, cred("cred-x", "acct"))
	e.waitNode(id, "cred-x applied", func(n testgateway.NodeInfo) bool { return n.AppliedVersion == v })
	if err := e.gw.SendFull(id); err != nil {
		t.Fatal(err)
	}
	gen := e.node(id).SessionGen
	// 全量之后到达的旧版本指令：v-1 的新增与 v 的移除都必须被忽略。
	mustSend(t, e.gw, id, &nodev1.Envelope{Body: &nodev1.Envelope_CredUpsert{CredUpsert: &nodev1.CredUpsert{ConfigVersion: v - 1, Credentials: []*nodev1.Credential{cred("cred-stale", "acct")}}}})
	mustSend(t, e.gw, id, &nodev1.Envelope{Body: &nodev1.Envelope_CredRemove{CredRemove: &nodev1.CredRemove{ConfigVersion: v, CredentialIds: []string{"cred-x"}}}})
	mustSend(t, e.gw, id, &nodev1.Envelope{Body: &nodev1.Envelope_SyncDelta{SyncDelta: &nodev1.SyncDelta{FromVersion: v - 2, ToVersion: v, Removals: []string{"cred-base"}}}})
	// 哨兵：之后的正常指令被应用，说明前面的旧指令已经处理完。
	v2 := e.gw.UpsertCredentials(id, cred("cred-z", "acct"))
	n := e.waitNode(id, "sentinel applied", func(n testgateway.NodeInfo) bool { return n.AppliedVersion == v2 })
	if n.SessionGen != gen {
		t.Errorf("agent reconnected (%d → %d); stale commands must not trigger a full sync", gen, n.SessionGen)
	}
	if st, ok := inspect(inst); ok {
		if slices.Contains(st.Credentials, "cred-stale") || !slices.Contains(st.Credentials, "cred-x") || !slices.Contains(st.Credentials, "cred-base") {
			t.Errorf("stale command applied: credentials %v", st.Credentials)
		}
		checkVersionsOnce(t, st.Versions)
	}
}

// TestAgent_VersionGapRequestsFull：增量指令跳号时不应用，断开后以 config_version=0 重连请求全量（NODE-23）。
func TestAgent_VersionGapRequestsFull(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, inst := e.start(startOpts{})
	v := e.configure(id)
	before := len(e.node(id).Handshakes)
	mustSend(t, e.gw, id, &nodev1.Envelope{Body: &nodev1.Envelope_CredUpsert{CredUpsert: &nodev1.CredUpsert{ConfigVersion: v + 2, Credentials: []*nodev1.Credential{cred("cred-gap", "acct")}}}})
	n := e.waitNode(id, "reconnect with config_version 0", func(n testgateway.NodeInfo) bool {
		for _, h := range n.Handshakes[before:] {
			if h.Reject == 0 && h.ConfigVersion == 0 {
				return n.Connected
			}
		}
		return false
	})
	if full := lastSync(n); full.Mode != nodev1.SyncMode_SYNC_MODE_FULL {
		t.Errorf("sync after full-sync request = %+v, want full", full)
	}
	n = e.converged(id)
	if st, ok := inspect(inst); ok && slices.Contains(st.Credentials, "cred-gap") {
		t.Errorf("gapped command applied: %v", st.Credentials)
	}
	// 请求已满足：之后的重连按正常版本同步。
	gen := n.SessionGen
	e.gw.DropSession(id)
	n = e.waitNode(id, "reconnect", func(n testgateway.NodeInfo) bool { return n.Connected && n.SessionGen > gen })
	if h := n.Handshakes[lastAccepted(n)]; h.ConfigVersion != n.Version {
		t.Errorf("after the full sync the agent reconnected with config_version %d, want %d", h.ConfigVersion, n.Version)
	}
}

// TestAgent_SnapshotChecksum：全量快照校验和不符时不应用，并请求全量（NODE-15、NODE-23）。
func TestAgent_SnapshotChecksum(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, inst := e.start(startOpts{})
	v := e.configure(id)
	env := e.gw.SnapshotEnvelope(id)
	sf := env.GetSyncFull()
	snap := new(nodev1.Snapshot)
	if err := proto.Unmarshal(sf.GetSnapshot(), snap); err != nil {
		t.Fatal(err)
	}
	snap.ConfigVersion = v + 5
	snap.Credentials = nil
	raw, _ := nodewire.MarshalDeterministic(snap)
	sf.Snapshot = raw // 校验和仍是原快照的
	before := len(e.node(id).Handshakes)
	mustSend(t, e.gw, id, env)
	e.waitNode(id, "full sync requested", func(n testgateway.NodeInfo) bool {
		for _, h := range n.Handshakes[before:] {
			if h.Reject == 0 && h.ConfigVersion == 0 {
				return n.Connected
			}
		}
		return false
	})
	n := e.converged(id)
	if n.AppliedVersion != v {
		t.Fatalf("applied version %d, want %d", n.AppliedVersion, v)
	}
	if st, ok := inspect(inst); ok && !slices.Contains(st.Credentials, "cred-base") {
		t.Errorf("tampered snapshot applied: %v", st.Credentials)
	}
}

// TestAgent_InvalidCredentialLength：长度不合法的凭据不加入任何入站，同一消息中的其他凭据照常应用，
// kernel_error 报告 "credential <id>: invalid length"；被正确的值替换后清空（AGT-15）。
func TestAgent_InvalidCredentialLength(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, inst := e.start(startOpts{})
	e.configure(id)
	bad := cred("cred-bad", "acct")
	bad.Secret = bad.Secret[:15]
	badKey := cred("cred-badkey", "acct")
	badKey.Ss2022Key_32 = nil
	v := e.gw.UpsertCredentials(id, bad, cred("cred-good", "acct"), badKey)
	want := "credential cred-bad: invalid length; credential cred-badkey: invalid length"
	n := e.waitNode(id, "kernel_error for invalid credentials", func(n testgateway.NodeInfo) bool {
		return n.AppliedVersion == v && n.LastStatus.GetKernelError() != ""
	})
	if got := n.LastStatus.GetKernelError(); got != want {
		t.Errorf("kernel_error = %q, want %q", got, want)
	}
	if st, ok := inspect(inst); ok {
		if slices.Contains(st.Credentials, "cred-bad") || slices.Contains(st.Credentials, "cred-badkey") || !slices.Contains(st.Credentials, "cred-good") {
			t.Errorf("credentials = %v", st.Credentials)
		}
	}
	// 一条被修正、一条被移除后，错误清空。
	e.gw.UpsertCredentials(id, cred("cred-bad", "acct"))
	v = e.gw.RemoveCredentials(id, "cred-badkey")
	e.waitNode(id, "kernel_error cleared", func(n testgateway.NodeInfo) bool {
		return n.AppliedVersion == v && n.LastStatus.GetKernelError() == ""
	})
}

// TestAgent_KernelSwitchAndCapabilities：按控制面选择的内核切换（AGT-07）；目标内核或协议
// 不在 Agent 上报的能力内时，保留原配置、不前进版本并报告错误（AGT-07、NODE-23）。
func TestAgent_KernelSwitchAndCapabilities(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, _ := e.start(startOpts{})
	v := e.configure(id)
	caps := e.node(id).Capabilities
	kernels := map[nodev1.KernelType]*nodev1.KernelSupport{}
	for _, k := range caps.GetKernels() {
		kernels[k.GetKernel()] = k
	}

	// 切换到另一个内置内核。
	for _, k := range slices.Sorted(maps.Keys(kernels)) {
		if k == caps.GetKernel() || !slices.Contains(kernels[k].GetStableProtocols(), nodev1.Protocol_PROTOCOL_VLESS) {
			continue
		}
		v = e.gw.SetInbounds(id, k, vlessTCP("vless-tcp"))
		n := e.waitNode(id, "kernel switched", func(n testgateway.NodeInfo) bool { return n.AppliedVersion == v })
		if got := n.LastStatus.GetRunningKernel(); got != k {
			t.Errorf("running_kernel = %s, want %s", got, k)
		}
		break
	}

	// 找一个不受支持的（内核, 协议）组合：未内置的内核，或内核不支持的协议。
	var badKernel nodev1.KernelType
	var badProto nodev1.Protocol
	for k := nodev1.KernelType_KERNEL_TYPE_SINGBOX; k <= nodev1.KernelType_KERNEL_TYPE_XRAY && badKernel == 0; k++ {
		ks := kernels[k]
		if ks == nil {
			badKernel, badProto = k, nodev1.Protocol_PROTOCOL_VLESS
			break
		}
		for p := nodev1.Protocol_PROTOCOL_VLESS; p <= nodev1.Protocol_PROTOCOL_ANYTLS; p++ {
			if !slices.Contains(ks.GetStableProtocols(), p) && !slices.Contains(ks.GetExperimentalProtocols(), p) {
				badKernel, badProto = k, p
				break
			}
		}
	}
	if badKernel == 0 {
		t.Log("agent supports every kernel and protocol; skipping the unsupported case")
		return
	}
	good := v
	bad := e.gw.SetInbounds(id, badKernel, &nodev1.Inbound{Tag: "bad", Protocol: badProto, Transport: nodev1.Transport_TRANSPORT_TCP, ListenPort: 8443})
	n := e.waitNode(id, "apply failure reported", func(n testgateway.NodeInfo) bool { return n.LastStatus.GetKernelError() != "" })
	if n.AppliedVersion != good {
		t.Errorf("applied version advanced to %d after a failed apply of %d (want %d)", n.AppliedVersion, bad, good)
	}
	// 修正配置后收敛。
	e.gw.SetInbounds(id, nodev1.KernelType_KERNEL_TYPE_SINGBOX, vlessTCP("vless-tcp"))
	e.waitNode(id, "recovered", func(n testgateway.NodeInfo) bool {
		return isConverged(n) && n.LastStatus.GetKernelError() == "" && n.State == testgateway.StateOnline
	})
}

func mustSend(t *testing.T, gw *testgateway.Server, id string, env *nodev1.Envelope) {
	t.Helper()
	if err := gw.SendEnvelope(id, env); err != nil {
		t.Fatal(err)
	}
}

func lastSync(n testgateway.NodeInfo) testgateway.SyncRecord {
	for i := len(n.Syncs) - 1; i >= 0; i-- {
		if n.Syncs[i].Reason == "handshake" {
			return n.Syncs[i]
		}
	}
	return testgateway.SyncRecord{}
}

// checkVersionsOnce 断言已应用的版本严格递增：每个版本至多应用一次（“不重”）。
func checkVersionsOnce(t *testing.T, versions []uint64) {
	t.Helper()
	for i := 1; i < len(versions); i++ {
		if versions[i] <= versions[i-1] {
			t.Fatalf("config versions applied out of order or twice: …%v", versions[max(0, i-3):i+1])
		}
	}
}

func checkSeqsIncreasing(t *testing.T, seqs []uint64) {
	t.Helper()
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("report_seq not strictly increasing: …%v", seqs[max(0, i-3):i+1])
		}
	}
}
