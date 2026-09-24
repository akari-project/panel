// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"context"
	"testing"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/fakeagent"
	"github.com/akari-project/panel/server/e2e/testgateway"
)

// TestAgent_StatusReports：ReportStatus 周期上报（NODE-20），包含已应用的 config_version、
// 当前运行的内核与每个入站的健康状态。
func TestAgent_StatusReports(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, _ := e.start(startOpts{})
	v := e.configure(id)
	n := e.node(id)
	st := n.LastStatus
	if st.GetConfigVersion() != v || st.GetRunningKernel() == nodev1.KernelType_KERNEL_TYPE_UNSPECIFIED {
		t.Errorf("status = %v, want config_version %d and a running kernel", st, v)
	}
	if len(st.GetInbounds()) != 1 || st.GetInbounds()[0].GetTag() != "vless-tcp" || !st.GetInbounds()[0].GetListening() {
		t.Errorf("inbound health = %v", st.GetInbounds())
	}
	// 周期上报：在两个周期（加余量）内至少再收到两份。
	count := n.Statuses
	e.waitNodeFor(3*e.tm.StatusInterval+e.tm.Patience/4, id, "periodic status", func(n testgateway.NodeInfo) bool { return n.Statuses >= count+2 })
}

// TestAgent_TrafficExactlyOnce：流量报告的 report_seq 从 HelloAck.last_report_seq 之后开始，跨断线与重启
// 严格递增（ACC-03）；断线与重启期间的报告从 WAL 重传（ACC-02），入账字节等于产生的字节（不丢不重）。
func TestAgent_TrafficExactlyOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	const watermark = 1000
	id, inst := e.start(startOpts{node: testgateway.NodeOptions{LastReportSeq: watermark}})
	ts := trafficSource(t, inst)
	e.configure(id)

	var up, down uint64
	send := func(n uint64) {
		ts.AddTraffic("cred-base", n, 2*n)
		ts.FlushTraffic()
		up, down = up+n, down+2*n
	}
	send(100)
	send(200)
	gen := e.node(id).SessionGen
	e.gw.PauseReading(id, true) // 报告写入 WAL 后在途，未被确认
	send(300)
	e.gw.DropSession(id)
	e.gw.PauseReading(id, false)
	e.waitNode(id, "reconnect", func(n testgateway.NodeInfo) bool { return n.Connected && n.SessionGen > gen })
	send(400)
	inst.Restart(t)
	send(500)
	n := e.waitNode(id, "all traffic ingested", func(n testgateway.NodeInfo) bool {
		return n.Totals["cred-base"] == testgateway.Totals{Up: up, Down: down}
	})
	if len(n.ReportSeqs) == 0 || n.ReportSeqs[0] <= watermark {
		t.Fatalf("first report_seq %v, want > last_report_seq %d", n.ReportSeqs, watermark)
	}
	checkSeqsIncreasing(t, n.ReportSeqs)
}

// TestAgent_ReinstallReportSeq：重新接入的新装 Agent 从控制面已入账的水位之后继续编号，
// 报告不被误判为重复（spec/22 22.6）。
func TestAgent_ReinstallReportSeq(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, old := e.start(startOpts{})
	ts := trafficSource(t, old)
	e.configure(id)
	ts.AddTraffic("cred-base", 10, 10)
	ts.FlushTraffic()
	n := e.waitNode(id, "first report", func(n testgateway.NodeInfo) bool { return n.Totals["cred-base"].Up == 10 })
	mark := n.LastReportSeq
	old.Stop()

	fresh := e.startWithToken(id, nil)
	ts2 := trafficSource(t, fresh)
	e.converged(id)
	ts2.AddTraffic("cred-base", 5, 5)
	ts2.FlushTraffic()
	n = e.waitNode(id, "report from reinstalled agent ingested", func(n testgateway.NodeInfo) bool { return n.Totals["cred-base"].Up == 15 })
	if n.LastReportSeq <= mark || n.DupReports != 0 || n.StaleReports != 0 {
		t.Errorf("report_seq %d after reinstall (mark %d), duplicates %d, stale %d", n.LastReportSeq, mark, n.DupReports, n.StaleReports)
	}
}

// TestAgent_ReportBackpressure：控制面不确认时，节点方向的在途报告不超过窗口，报告不丢弃，
// 恢复后全部送达（NODE-14）。
func TestAgent_ReportBackpressure(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{})
	id, inst := e.start(startOpts{})
	ts := trafficSource(t, inst)
	e.configure(id)
	e.gw.PauseReading(id, true)
	reports := e.tm.WindowMessages + 20
	for range reports {
		ts.AddTraffic("cred-base", 1, 0)
		ts.FlushTraffic()
	}
	e.gw.PauseReading(id, false)
	n := e.waitNode(id, "all reports delivered", func(n testgateway.NodeInfo) bool { return n.Totals["cred-base"].Up == uint64(reports) })
	if len(n.ReportSeqs) != reports {
		t.Errorf("ingested %d reports, want %d", len(n.ReportSeqs), reports)
	}
}

// TestAgent_LeaseLifecycle：首个连接申请租约，余额低于 20% 时续租，最后一个连接关闭时释放（ACC-08–10）。
func TestAgent_LeaseLifecycle(t *testing.T) {
	t.Parallel()
	const lease = 1 << 20
	e := newEnv(t, testgateway.Config{LeaseBytes: lease})
	id, inst := e.start(startOpts{})
	cs := connectionSource(t, inst)
	ts := trafficSource(t, inst)
	e.configure(id)
	e.gw.UpsertCredentials(id, cred("cred-l", "acct-l"))
	e.converged(id)

	cs.OpenConnection("acct-l")
	n := e.waitNode(id, "initial lease request", func(n testgateway.NodeInfo) bool { return len(n.LeaseRequests) == 1 })
	if r := n.LeaseRequests[0]; r.GetAccountId() != "acct-l" || r.GetCurrentLeaseId() != "" || r.GetIsRelease() {
		t.Fatalf("initial request = %v", r)
	}
	cs.OpenConnection("acct-l") // 第二条连接不再申请
	// 等租约到达后消耗 85%，触发续租。
	waitLeaseGranted(t, e, inst, 1)
	ts.AddTraffic("cred-l", lease*85/100, 0)
	n = e.waitNode(id, "renewal request", func(n testgateway.NodeInfo) bool { return len(n.LeaseRequests) == 2 })
	renew := n.LeaseRequests[1]
	if renew.GetCurrentLeaseId() == "" || renew.GetIsRelease() || renew.GetRemainingBytes() <= 0 || renew.GetRemainingBytes() >= lease/5 {
		t.Fatalf("renewal = %v, want current_lease_id and remaining < 20%%", renew)
	}
	waitLeaseGranted(t, e, inst, 2)
	cs.CloseConnection("acct-l")
	cs.CloseConnection("acct-l")
	n = e.waitNode(id, "release request", func(n testgateway.NodeInfo) bool { return len(n.LeaseRequests) == 3 })
	rel := n.LeaseRequests[2]
	if !rel.GetIsRelease() || rel.GetCurrentLeaseId() == "" || rel.GetCurrentLeaseId() == renew.GetCurrentLeaseId() {
		t.Fatalf("release = %v, want is_release with the latest lease id (not %q)", rel, renew.GetCurrentLeaseId())
	}
}

// TestAgent_NoReleaseWithoutCapability：控制面未声明 supports_lease_release 时，Agent 不发送释放（ACC-10、NODE-16）。
func TestAgent_NoReleaseWithoutCapability(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testgateway.Config{DisableLeaseRelease: true})
	id, inst := e.start(startOpts{})
	cs := connectionSource(t, inst)
	e.configure(id)
	cs.OpenConnection("acct-n")
	e.waitNode(id, "lease request", func(n testgateway.NodeInfo) bool { return len(n.LeaseRequests) == 1 })
	waitLeaseGranted(t, e, inst, 1)
	cs.CloseConnection("acct-n")
	// 以一次状态上报作为时间参照：关闭连接之后的处理已经完成。
	count := e.node(id).Statuses
	n := e.waitNodeFor(3*e.tm.StatusInterval+e.tm.Patience/4, id, "status after close", func(n testgateway.NodeInfo) bool { return n.Statuses > count })
	if len(n.LeaseRequests) != 1 || n.UnexpectedReleases != 0 {
		t.Fatalf("lease requests %v, unexpected releases %d", n.LeaseRequests, n.UnexpectedReleases)
	}
}

// waitLeaseGranted 等待被测实现收到第 count 份租约。只有模拟 Agent 能观察到；其他实现不等待。
func waitLeaseGranted(t *testing.T, e *env, inst Instance, count int) {
	t.Helper()
	fi, ok := inst.(*FakeInstance)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), e.tm.Patience)
	defer cancel()
	seen := 0
	if _, ok := fi.Agent().WaitEvent(ctx, func(ev fakeagent.Event) bool {
		if ev.Kind == fakeagent.EventLease && ev.Seq > 0 {
			seen++
		}
		return seen >= count
	}); !ok {
		t.Fatal("lease not granted")
	}
}
