// SPDX-License-Identifier: AGPL-3.0-or-later

package fakeagent

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/nodewire"
)

// onEnvelope 在会话读协程中处理控制面下发的信封。
func (a *Agent) onEnvelope(sess *nodewire.Session, env *nodev1.Envelope) {
	a.mu.Lock()
	defer a.mu.Unlock()
	idem := env.GetIdemKey()
	switch b := env.GetBody().(type) {
	case *nodev1.Envelope_SyncFull:
		a.applyFullLocked(sess, idem, b.SyncFull)
	case *nodev1.Envelope_SyncDelta:
		d := b.SyncDelta
		switch {
		case d.GetToVersion() <= a.st.ConfigVersion:
			a.ignoredLocked("sync_delta", d.GetToVersion(), "not newer than local")
		case d.GetFromVersion() == a.st.ConfigVersion:
			a.upsertCredsLocked(d.GetUpserts())
			a.removeCredsLocked(d.GetRemovals())
			a.advanceLocked("sync_delta", d.GetToVersion(), idem)
		default:
			a.requestFullLocked(sess, fmt.Sprintf("sync_delta %d→%d does not start at local %d", d.GetFromVersion(), d.GetToVersion(), a.st.ConfigVersion))
		}
	case *nodev1.Envelope_CredUpsert:
		if a.checkVersionLocked(sess, "cred_upsert", b.CredUpsert.GetConfigVersion()) {
			a.upsertCredsLocked(b.CredUpsert.GetCredentials())
			a.advanceLocked("cred_upsert", b.CredUpsert.GetConfigVersion(), idem)
		}
	case *nodev1.Envelope_CredRemove:
		if a.checkVersionLocked(sess, "cred_remove", b.CredRemove.GetConfigVersion()) {
			a.removeCredsLocked(b.CredRemove.GetCredentialIds())
			a.advanceLocked("cred_remove", b.CredRemove.GetConfigVersion(), idem)
		}
	case *nodev1.Envelope_InboundApply:
		ia := b.InboundApply
		if a.checkVersionLocked(sess, "inbound_apply", ia.GetConfigVersion()) {
			kernel, err := a.checkInboundsLocked(ia.GetKernel(), ia.GetInbounds())
			if err != nil {
				a.applyFailedLocked(sess, "inbound_apply", ia.GetConfigVersion(), err)
				return
			}
			a.st.Kernel, a.st.Inbounds = kernel, ia.GetInbounds()
			a.applyErr = ""
			a.advanceLocked("inbound_apply", ia.GetConfigVersion(), idem)
		}
	case *nodev1.Envelope_RoutesApply:
		ra := b.RoutesApply
		if a.checkVersionLocked(sess, "routes_apply", ra.GetConfigVersion()) {
			if err := a.checkDNSSecret(ra.GetDnsProviderSecret()); err != nil {
				a.applyFailedLocked(sess, "routes_apply", ra.GetConfigVersion(), err)
				return
			}
			a.st.RoutesJSON, a.st.DNSProviderSecret = ra.GetRoutesJson(), ra.GetDnsProviderSecret()
			a.advanceLocked("routes_apply", ra.GetConfigVersion(), idem)
		}
	case *nodev1.Envelope_QuotaLease:
		a.onLeaseLocked(b.QuotaLease)
	case *nodev1.Envelope_AgentUpgrade:
		u := b.AgentUpgrade
		detail := "unverified"
		if len(a.cfg.UpgradeKeys) > 0 {
			detail = "signature ok"
			if !nodewire.VerifyAgentUpgrade(a.cfg.UpgradeKeys, u) {
				detail = "signature invalid"
			}
		}
		a.upgrades = append(a.upgrades, u)
		a.events.add(Event{Kind: EventUpgrade, At: a.clock.Now(), Detail: u.GetVersion() + ": " + detail})
	case *nodev1.Envelope_SourceSet:
		a.sourceSets[b.SourceSet.GetCredentialId()] = b.SourceSet
	default:
		// 节点 → 控制面方向的消息或未知字段：确认后忽略（NODE-17 只增字段）。
	}
}

// checkVersionLocked 实现增量指令的版本规则（NODE-23）：不大于本地的只确认；等于本地加 1 的应用；
// 跳号的不应用，改为请求全量同步。
func (a *Agent) checkVersionLocked(sess *nodewire.Session, body string, v uint64) bool {
	switch {
	case v <= a.st.ConfigVersion:
		a.ignoredLocked(body, v, "not newer than local")
		return false
	case v == a.st.ConfigVersion+1:
		return true
	default:
		a.requestFullLocked(sess, fmt.Sprintf("%s version %d skips local %d", body, v, a.st.ConfigVersion))
		return false
	}
}

func (a *Agent) applyFullLocked(sess *nodewire.Session, idem string, sf *nodev1.SyncFull) {
	if !nodewire.VerifySnapshot(sf) {
		a.requestFullLocked(sess, "sync_full checksum mismatch")
		return
	}
	snap := new(nodev1.Snapshot)
	if err := proto.Unmarshal(sf.GetSnapshot(), snap); err != nil {
		a.requestFullLocked(sess, "sync_full snapshot does not decode")
		return
	}
	v := snap.GetConfigVersion()
	if v <= a.st.ConfigVersion {
		if v == a.st.ConfigVersion {
			// 本地已与控制面一致，之前的全量同步请求（跳号、校验失败）已经满足。
			a.wantFull, a.fullAttempts = false, 0
		}
		a.ignoredLocked("sync_full", v, "not newer than local")
		return
	}
	kernel, err := a.checkInboundsLocked(snap.GetKernel(), snap.GetInbounds())
	if err == nil {
		err = a.checkDNSSecret(snap.GetDnsProviderSecret())
	}
	if err != nil {
		a.applyFailedLocked(sess, "sync_full", v, err)
		return
	}
	// 原子替换本地快照（NODE-15）：不在快照中的凭据一律移除（NODE-24）。
	a.st.Snapshot = sf.GetSnapshot()
	a.st.Kernel, a.st.Inbounds = kernel, snap.GetInbounds()
	a.st.Credentials = make(map[string]*nodev1.Credential)
	clear(a.credErrs)
	a.upsertCredsLocked(snap.GetCredentials())
	a.st.RoutesJSON, a.st.DNSProviderSecret = snap.GetRoutesJson(), snap.GetDnsProviderSecret()
	a.st.OfflinePolicy = snap.GetOfflinePolicy()
	a.applyErr = ""
	a.wantFull, a.fullAttempts = false, 0
	a.advanceLocked("sync_full", v, idem)
	// 收到全量后，为仍有连接的账号重新申请租约（NODE-14、ACC-08）。
	for _, acct := range slices.Sorted(maps.Keys(a.conns)) {
		if a.conns[acct] > 0 {
			a.leaseRequestLocked(acct, false)
		}
	}
}

// checkInboundsLocked 确认内核在本二进制中，且每个入站的协议与传输在该内核的能力内（AGT-07、AGT-08）。
func (a *Agent) checkInboundsLocked(kernel nodev1.KernelType, inbounds []*nodev1.Inbound) (nodev1.KernelType, error) {
	if kernel == nodev1.KernelType_KERNEL_TYPE_UNSPECIFIED {
		kernel = a.st.Kernel
	}
	ks := a.kernels[kernel]
	if ks == nil {
		return 0, fmt.Errorf("kernel %s is not built in", kernel)
	}
	for _, in := range inbounds {
		if !slices.Contains(ks.GetStableProtocols(), in.GetProtocol()) && !slices.Contains(ks.GetExperimentalProtocols(), in.GetProtocol()) {
			return 0, fmt.Errorf("inbound %s: protocol %s not supported by %s", in.GetTag(), in.GetProtocol(), kernel)
		}
		if t := in.GetTransport(); t != nodev1.Transport_TRANSPORT_UNSPECIFIED && !slices.Contains(ks.GetStableTransports(), t) && !slices.Contains(ks.GetExperimentalTransports(), t) {
			return 0, fmt.Errorf("inbound %s: transport %s not supported by %s", in.GetTag(), t, kernel)
		}
	}
	return kernel, nil
}

// checkDNSSecret 用当前 PSK 解密 DNS 服务商凭据，只在内存中使用（NODE-25）。
func (a *Agent) checkDNSSecret(sealed []byte) error {
	if len(sealed) == 0 {
		return nil
	}
	if _, err := nodewire.OpenDNSSecret(a.cfg.PSK, sealed); err != nil {
		return fmt.Errorf("dns provider secret: %w", err)
	}
	return nil
}

// upsertCredsLocked 加入或替换凭据。长度不合法的凭据不加入，并在 kernel_error 中报告（AGT-15）。
func (a *Agent) upsertCredsLocked(creds []*nodev1.Credential) {
	for _, c := range creds {
		if len(c.GetSecret()) != 16 || len(c.GetSs2022Key_16()) != 16 || len(c.GetSs2022Key_32()) != 32 {
			a.credErrs[c.GetId()] = struct{}{}
			continue
		}
		delete(a.credErrs, c.GetId())
		a.st.Credentials[c.GetId()] = c
	}
}

// removeCredsLocked 移除凭据；真实 Agent 在 1 秒内关闭其全部连接（NODE-24）。
func (a *Agent) removeCredsLocked(ids []string) {
	for _, id := range ids {
		delete(a.st.Credentials, id)
		delete(a.credErrs, id)
	}
}

func (a *Agent) advanceLocked(body string, v uint64, idem string) {
	a.st.ConfigVersion = v
	a.applied = append(a.applied, Applied{Body: body, Version: v, IdemKey: idem})
	a.events.add(Event{Kind: EventApplied, At: a.clock.Now(), Body: body, Version: v})
	a.sendStatusLocked()
}

func (a *Agent) ignoredLocked(body string, v uint64, why string) {
	a.events.add(Event{Kind: EventIgnored, At: a.clock.Now(), Body: body, Version: v, Detail: why})
}

// applyFailedLocked 保留原配置、不前进版本，报告错误并请求全量同步（NODE-23、AGT-07）。
func (a *Agent) applyFailedLocked(sess *nodewire.Session, body string, v uint64, err error) {
	a.applyErr = err.Error()
	a.events.add(Event{Kind: EventApplyFailed, At: a.clock.Now(), Body: body, Version: v, Detail: a.applyErr})
	a.sendStatusLocked()
	a.requestFullLocked(sess, body+" apply failed")
}

// requestFullLocked 断开连接，之后以 config_version=0 重连（NODE-23）。
func (a *Agent) requestFullLocked(sess *nodewire.Session, why string) {
	a.wantFull = true
	a.log.Info("fakeagent: requesting full sync", "why", why)
	a.events.add(Event{Kind: EventFullRequested, At: a.clock.Now(), Detail: why})
	sess.Close(nodewire.CloseNormal, "full sync requested")
}

// kernelErrorLocked 组合 ReportStatus.kernel_error：最近一次失败原因，加上凭据校验错误
// （"credential <id>: invalid length"，多条以分号分隔，不含秘密值）。
func (a *Agent) kernelErrorLocked() string {
	var parts []string
	if a.applyErr != "" {
		parts = append(parts, a.applyErr)
	}
	for _, id := range slices.Sorted(maps.Keys(a.credErrs)) {
		parts = append(parts, "credential "+id+": invalid length")
	}
	return strings.Join(parts, "; ")
}

func (a *Agent) sendStatusLocked() {
	if a.sess == nil {
		return
	}
	var conns uint32
	for _, n := range a.conns {
		conns += uint32(n)
	}
	st := &nodev1.ReportStatus{
		CpuPercent:     1.5,
		MemUsedBytes:   24 << 20,
		MemTotalBytes:  1 << 30,
		TcpConnections: conns,
		UptimeSeconds:  uint64(a.clock.Now().Sub(a.started).Seconds()),
		ConfigVersion:  a.st.ConfigVersion,
		RunningKernel:  a.st.Kernel,
		KernelError:    a.kernelErrorLocked(),
	}
	for _, in := range a.st.Inbounds {
		errText := a.cfg.InboundFailures[in.GetTag()]
		st.Inbounds = append(st.Inbounds, &nodev1.InboundHealth{Tag: in.GetTag(), Listening: errText == "", Error: errText})
	}
	env := a.newEnvelope()
	env.Body = &nodev1.Envelope_ReportStatus{ReportStatus: st}
	o := nodewire.NewOutgoing(env, statusTag{})
	o.Key = "status" // 未发送的状态只保留最新一条（NODE-14 背压下队列有上限）
	a.sendLocked(o)
}

// housekeeping 执行本地兜底：凭据到期移除（NODE-24）、租约过期（ACC-10）、首连租约等待超时（ACC-08）。
func (a *Agent) housekeeping() {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock.Now()
	for id, c := range a.st.Credentials {
		if ms := c.GetExpiresAtMs(); ms > 0 && now.UnixMilli() >= ms {
			delete(a.st.Credentials, id)
			a.events.add(Event{Kind: EventCredExpired, At: now, Detail: id})
		}
	}
	for acct, l := range a.st.Leases {
		if !l.ExpiresAt.IsZero() && !now.Before(l.ExpiresAt) {
			delete(a.st.Leases, acct)
		}
	}
	for acct, since := range a.leaseWait {
		if now.Sub(since) >= a.timing.LeaseGrace && a.st.Leases[acct] == nil {
			delete(a.leaseWait, acct)
			a.closeConnsLocked(acct, "lease not granted in time")
		}
	}
}

// View 是模拟 Agent 当前状态的快照，供测试断言（conformance 的 StateInspector）。
type View struct {
	Connected     bool
	Handshakes    int
	ConfigVersion uint64
	Kernel        nodev1.KernelType
	Inbounds      []string // tag
	Credentials   []string // id，已排序
	KernelError   string
	WantFull      bool
	NextReportSeq uint64
	WAL           int
	Leases        map[string]Lease
	Connections   map[string]int
	Applied       []Applied
	Upgrades      int
	// ServerSupportsLeaseRelease 取自最近一次 HelloAck 的控制面能力位。
	ServerSupportsLeaseRelease bool
}

// View 返回当前状态。
func (a *Agent) View() View {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := View{
		Connected:     a.sess != nil,
		Handshakes:    a.handshakes,
		ConfigVersion: a.st.ConfigVersion,
		Kernel:        a.st.Kernel,
		Credentials:   slices.Sorted(maps.Keys(a.st.Credentials)),
		KernelError:   a.kernelErrorLocked(),
		WantFull:      a.wantFull,
		NextReportSeq: a.st.NextReportSeq,
		WAL:           len(a.st.WAL),
		Leases:        make(map[string]Lease, len(a.st.Leases)),
		Connections:   maps.Clone(a.conns),
		Applied:       slices.Clone(a.applied),
		Upgrades:      len(a.upgrades),
	}
	for _, in := range a.st.Inbounds {
		v.Inbounds = append(v.Inbounds, in.GetTag())
	}
	for k, l := range a.st.Leases {
		v.Leases[k] = *l
	}
	if a.serverCaps != nil {
		v.ServerSupportsLeaseRelease = a.serverCaps.GetSupportsLeaseRelease()
	}
	return v
}
