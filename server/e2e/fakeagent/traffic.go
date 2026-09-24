// SPDX-License-Identifier: AGPL-3.0-or-later

package fakeagent

import (
	"maps"
	"slices"
	"sync/atomic"
	"time"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/nodewire"
)

// counter 是一条凭据的计数器，按 ACC-01 以原子交换读出并清零。
type counter struct {
	up, down atomic.Uint64
}

// AddTraffic 为凭据累加代理载荷字节（模拟内核计量，spec/21 AGT-12），并扣减所属账号的租约（ACC-09）。
func (a *Agent) AddTraffic(credID string, up, down uint64) {
	a.cmu.Lock()
	c := a.counters[credID]
	if c == nil {
		c = new(counter)
		a.counters[credID] = c
	}
	// 在锁内累加：FlushTraffic 在锁内删除空闲的计数器，二者不会交错。
	c.up.Add(up)
	c.down.Add(down)
	a.cmu.Unlock()

	a.mu.Lock()
	defer a.mu.Unlock()
	if cred := a.st.Credentials[credID]; cred != nil {
		a.consumeLocked(cred.GetAccountId(), int64(up+down))
	}
}

// FlushTraffic 立即读出计数器，生成一份 ReportTraffic 写入 WAL 并发送（spec/22 22.1 第 1 步）。
// 计数全为 0 时不生成报告。尚未取得 report_seq 水位的新装 Agent 暂不生成，计数保留到下次。
func (a *Agent) FlushTraffic() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.st.ReportSeqKnown {
		return
	}
	a.cmu.Lock()
	var items []*nodev1.TrafficItem
	for _, id := range slices.Sorted(maps.Keys(a.counters)) {
		c := a.counters[id]
		up, down := c.up.Swap(0), c.down.Swap(0)
		if up != 0 || down != 0 {
			items = append(items, &nodev1.TrafficItem{CredentialId: id, RawUp: up, RawDown: down})
		} else {
			delete(a.counters, id) // 一个周期内没有流量：移除，计数器表不随历史凭据增长
		}
	}
	a.cmu.Unlock()
	if len(items) == 0 {
		return
	}
	now := a.clock.Now()
	seq := a.st.NextReportSeq
	a.st.NextReportSeq++
	env := a.newEnvelope()
	env.Body = &nodev1.Envelope_ReportTraffic{ReportTraffic: &nodev1.ReportTraffic{
		ReportSeq:     seq,
		WindowStartMs: now.Add(-a.timing.TrafficInterval).UnixMilli(),
		WindowEndMs:   now.UnixMilli(),
		Items:         items,
	}}
	// 先写 WAL 再发送；确认后才移出（ACC-02）。
	a.st.WAL[seq] = env
	a.sendLocked(nodewire.NewOutgoing(env, reportTag(seq)))
}

// PendingTraffic 返回尚未写入报告的计数之和。
func (a *Agent) PendingTraffic() (up, down uint64) {
	a.cmu.Lock()
	defer a.cmu.Unlock()
	for _, c := range a.counters {
		up += c.up.Load()
		down += c.down.Load()
	}
	return up, down
}

// OpenConnection 模拟账号在本节点建立一条代理连接。账号的首个连接触发 LeaseRequest（ACC-08）。
// 租约已耗尽且续租未获批时拒绝新连接（ACC-09），返回 false。
func (a *Agent) OpenConnection(account string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	l := a.st.Leases[account]
	if l != nil && l.Remaining <= 0 {
		return false
	}
	a.conns[account]++
	if a.conns[account] == 1 && l == nil {
		a.leaseWait[account] = a.clock.Now()
		a.leaseRequestLocked(account, false)
	}
	return true
}

// CloseConnection 模拟关闭一条代理连接。最后一条连接关闭时，若控制面声明了 supports_lease_release，
// 发送带释放标志的 LeaseRequest（ACC-10）。
func (a *Agent) CloseConnection(account string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conns[account] == 0 {
		return
	}
	a.conns[account]--
	if a.conns[account] > 0 {
		return
	}
	delete(a.conns, account)
	delete(a.leaseWait, account)
	if a.st.Leases[account] != nil && a.serverCaps.GetSupportsLeaseRelease() {
		a.leaseRequestLocked(account, true)
	}
}

// consumeLocked 扣减租约：余额低于 20% 时续租；耗尽且续租未获批时关闭该账号的全部连接（ACC-09）。
func (a *Agent) consumeLocked(account string, n int64) {
	l := a.st.Leases[account]
	if l == nil {
		return
	}
	l.Remaining -= n
	if !l.renewing && l.Remaining*5 < l.Granted {
		l.renewing = true
		a.leaseRequestLocked(account, false)
	}
	if l.Remaining <= 0 && a.conns[account] > 0 {
		a.closeConnsLocked(account, "lease exhausted")
	}
}

func (a *Agent) closeConnsLocked(account, why string) {
	delete(a.conns, account)
	a.events.add(Event{Kind: EventConnsClosed, At: a.clock.Now(), Account: account, Detail: why})
}

func (a *Agent) leaseRequestLocked(account string, release bool) {
	req := &nodev1.LeaseRequest{AccountId: account, IsRelease: release}
	if l := a.st.Leases[account]; l != nil {
		req.CurrentLeaseId, req.RemainingBytes = l.ID, max(l.Remaining, 0)
	}
	env := a.newEnvelope()
	env.Body = &nodev1.Envelope_LeaseRequest{LeaseRequest: req}
	a.events.add(Event{Kind: EventLeaseRequest, At: a.clock.Now(), Account: account, Detail: leaseDetail(req)})
	o := nodewire.NewOutgoing(env, nil)
	o.Key = "lease:" + account // 同一账号未发送的请求合并为最新一条
	a.sendLocked(o)
}

func leaseDetail(r *nodev1.LeaseRequest) string {
	switch {
	case r.GetIsRelease():
		return "release"
	case r.GetCurrentLeaseId() != "":
		return "renew"
	default:
		return "initial"
	}
}

func (a *Agent) onLeaseLocked(q *nodev1.QuotaLease) {
	acct := q.GetAccountId()
	a.events.add(Event{Kind: EventLease, At: a.clock.Now(), Account: acct, Detail: q.GetLeaseId(), Seq: uint64(max(q.GetBytes(), 0))})
	if q.GetBytes() <= 0 {
		delete(a.st.Leases, acct)
		return
	}
	a.st.Leases[acct] = &Lease{
		ID:        q.GetLeaseId(),
		Granted:   q.GetBytes(),
		Remaining: q.GetBytes(),
		ExpiresAt: time.UnixMilli(q.GetExpiresAtMs()),
	}
	delete(a.leaseWait, acct)
}
