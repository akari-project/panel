// SPDX-License-Identifier: AGPL-3.0-or-later

package testgateway

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/nodewire"
)

// 节点状态（NODE-20）。
const (
	StatePendingEnroll = "pending_enroll"
	StatePendingConfig = "pending_config"
	StateOnline        = "online"
	StateOffline       = "offline"
)

// change 是一次配置变更；credOnly 表示只含凭据变化，可以用增量表达（NODE-15）。
type change struct {
	version  uint64
	at       time.Time
	credOnly bool
	credIDs  []string
}

type session struct {
	s       *nodewire.Session
	psk     []byte // 本会话通过验证的那一把（NODE-10、NODE-25）
	prevPSK bool
	gen     int
}

type rejectPlan struct {
	reason     nodev1.HelloRejectReason
	retryAfter uint32
}

type grant struct {
	id      string
	bytes   int64
	expires time.Time
}

type node struct {
	id string

	psk, pskPrev    []byte
	transitionUntil time.Time
	revoked         bool
	tokenHash       [32]byte
	tokenExpires    time.Time
	tokenUsedAt     time.Time
	tokenFP         string
	tokenResp       []byte

	version  uint64
	kernel   nodev1.KernelType
	inbounds []*nodev1.Inbound
	creds    map[string]*nodev1.Credential
	routes   []byte
	dns      []byte // DNS 服务商凭据明文，下发时按会话 PSK 加密（NODE-25）
	offline  *nodev1.OfflinePolicy
	changes  []change

	sess      *session
	gen       int
	lastSeen  time.Time
	status    *nodev1.ReportStatus
	statusGen int // 最近一次 ReportStatus 所在会话的代次
	statuses  int
	caps      *nodev1.Capabilities
	dedup     *nodewire.Dedup
	faults    nodewire.FaultSwitch
	gate      *gate
	rejects   []rejectPlan

	handshakes []Handshake
	syncs      []SyncRecord
	closes     []CloseRecord
	overflows  int

	baseReportSeq uint64 // 去重键已过期部分的水位（ACC-03）
	lastReportSeq uint64
	ingested      map[uint64]bool
	reportSeqs    []uint64
	totals        map[string]Totals
	dupReports    int
	staleReports  int

	leaseReqs          []*nodev1.LeaseRequest
	leaseResp          map[string]*nodev1.QuotaLease
	leases             map[string]grant
	unexpectedReleases int
}

// Handshake 记录一次握手尝试。
type Handshake struct {
	At            time.Time
	Nonce         []byte
	ConfigVersion uint64
	ProtoVersion  uint32
	// Reject 为 0 表示握手成功。
	Reject   nodev1.HelloRejectReason
	Injected bool // 由 InjectReject 注入的拒绝
	PrevPSK  bool // 以过渡期内的旧 PSK 通过验证
	Raw      []byte
}

// SyncRecord 记录一次同步下发。
type SyncRecord struct {
	At       time.Time
	Mode     nodev1.SyncMode
	From, To uint64
	Reason   string // handshake、overflow、manual
}

// CloseRecord 记录一次会话结束。
type CloseRecord struct {
	At  time.Time
	Gen int
	Err string
}

// Totals 是一条凭据已入账的原始字节。
type Totals struct{ Up, Down uint64 }

// NodeInfo 是节点状态的副本。
type NodeInfo struct {
	ID             string
	State          string
	Version        uint64 // 控制面当前的 config_version
	AppliedVersion uint64 // 最近一次 ReportStatus 的 config_version
	StatusGen      int    // 最近一次 ReportStatus 所在会话的代次；等于 SessionGen 表示来自当前会话
	Connected      bool
	SessionGen     int // 成功握手的次数
	HasPrevPSK     bool
	Revoked        bool
	LastStatus     *nodev1.ReportStatus
	Statuses       int
	Capabilities   *nodev1.Capabilities
	Handshakes     []Handshake
	Syncs          []SyncRecord
	Closes         []CloseRecord
	Overflows      int

	LastReportSeq uint64
	ReportSeqs    []uint64 // 已入账的 report_seq，按到达顺序
	Totals        map[string]Totals
	DupReports    int
	StaleReports  int

	LeaseRequests      []*nodev1.LeaseRequest
	UnexpectedReleases int

	Credentials []string // 控制面期望的凭据 ID，已排序
}

// Summary 是节点的简要状态（不复制切片）。
type Summary struct {
	ID             string
	State          string
	Version        uint64
	AppliedVersion uint64
	StatusGen      int
	Connected      bool
	SessionGen     int
}

// NodeOptions 是建档参数（NODE-01）。
type NodeOptions struct {
	ID            string            // 为空时生成 UUIDv7
	Kernel        nodev1.KernelType // 默认 sing-box（ADR 0003）
	LastReportSeq uint64            // 已入账的 report_seq 水位，模拟重装前的历史（ACC-03）
}

func (g *Gateway) infoLocked(n *node) NodeInfo {
	info := NodeInfo{
		ID:                 n.id,
		State:              g.stateLocked(n),
		Version:            n.version,
		Connected:          n.sess != nil,
		SessionGen:         n.gen,
		HasPrevPSK:         n.pskPrev != nil,
		Revoked:            n.revoked,
		Statuses:           n.statuses,
		StatusGen:          n.statusGen,
		Capabilities:       n.caps,
		Handshakes:         slices.Clone(n.handshakes),
		Syncs:              slices.Clone(n.syncs),
		Closes:             slices.Clone(n.closes),
		Overflows:          n.overflows,
		LastReportSeq:      n.lastReportSeq,
		ReportSeqs:         slices.Clone(n.reportSeqs),
		Totals:             maps.Clone(n.totals),
		DupReports:         n.dupReports,
		StaleReports:       n.staleReports,
		LeaseRequests:      slices.Clone(n.leaseReqs),
		UnexpectedReleases: n.unexpectedReleases,
		Credentials:        slices.Sorted(maps.Keys(n.creds)),
	}
	if n.status != nil {
		info.LastStatus = n.status
		info.AppliedVersion = n.status.GetConfigVersion()
	}
	return info
}

func (g *Gateway) summaryLocked(n *node) Summary {
	s := Summary{ID: n.id, State: g.stateLocked(n), Version: n.version, Connected: n.sess != nil, SessionGen: n.gen, StatusGen: n.statusGen}
	if n.status != nil {
		s.AppliedVersion = n.status.GetConfigVersion()
	}
	return s
}

// stateLocked 按 NODE-20 计算节点状态。
func (g *Gateway) stateLocked(n *node) string {
	if n.psk == nil {
		return StatePendingEnroll
	}
	if n.lastSeen.IsZero() {
		return StatePendingConfig
	}
	if g.cfg.Clock.Now().Sub(n.lastSeen) > g.cfg.OfflineWindow {
		return StateOffline
	}
	ready := false
	if n.status != nil && n.status.GetConfigVersion() == n.version {
		for _, h := range n.status.GetInbounds() {
			ready = ready || h.GetListening()
		}
	}
	if ready {
		return StateOnline
	}
	return StatePendingConfig
}

// Node 返回节点信息。
func (g *Gateway) Node(id string) (NodeInfo, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return View{g}.Node(id)
}

// CreateNode 建档，返回节点 ID；节点处于 pending_enroll（NODE-01）。
func (g *Gateway) CreateNode(opts NodeOptions) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	id := opts.ID
	if id == "" {
		id = nodewire.NewUUIDv7(g.cfg.Clock, g.cfg.Rand)
	}
	k := opts.Kernel
	if k == nodev1.KernelType_KERNEL_TYPE_UNSPECIFIED {
		k = nodev1.KernelType_KERNEL_TYPE_SINGBOX
	}
	g.nodes[id] = &node{
		id:            id,
		kernel:        k,
		creds:         make(map[string]*nodev1.Credential),
		offline:       &nodev1.OfflinePolicy{OfflineLeaseBytes: 64 << 20, OfflineMaxSeconds: 86400},
		dedup:         nodewire.NewDedup(g.cfg.Clock, 0),
		baseReportSeq: opts.LastReportSeq,
		lastReportSeq: opts.LastReportSeq,
		ingested:      make(map[uint64]bool),
		totals:        make(map[string]Totals),
		leaseResp:     make(map[string]*nodev1.QuotaLease),
		leases:        make(map[string]grant),
		gate:          newGate(),
	}
	g.notifyLocked()
	return id
}

// IssueEnrollToken 签发一次性接入令牌（NODE-01、NODE-03）：24 小时有效，只保存哈希。
// 重新签发时保留节点 ID 与配置。
func (g *Gateway) IssueEnrollToken(id string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.mustNodeLocked(id)
	b := make([]byte, 32)
	if _, err := io.ReadFull(g.cfg.Rand, b); err != nil {
		panic(err)
	}
	token := fmt.Sprintf("%x", b)
	n.tokenHash = sha256.Sum256([]byte(token))
	n.tokenExpires = g.cfg.Clock.Now().Add(g.cfg.EnrollTokenTTL)
	n.tokenUsedAt, n.tokenFP, n.tokenResp = time.Time{}, "", nil
	return token
}

// Provision 不经接入接口直接为节点设定 PSK 并返回（用于规模测试与网关断言）。
func (g *Gateway) Provision(id string) []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.mustNodeLocked(id)
	psk := make([]byte, nodewire.PSKSize)
	if _, err := io.ReadFull(g.cfg.Rand, psk); err != nil {
		panic(err)
	}
	g.setPSKLocked(n, psk)
	return psk
}

// setPSKLocked 写入新 PSK；已有 PSK 时移到 psk_prev，进入过渡期（NODE-03）。
func (g *Gateway) setPSKLocked(n *node, psk []byte) {
	if n.psk != nil {
		n.pskPrev = n.psk
		n.transitionUntil = g.cfg.Clock.Now().Add(g.cfg.TransitionPeriod)
	}
	n.psk = psk
	n.revoked = false
	g.notifyLocked()
}

func (g *Gateway) mustNodeLocked(id string) *node {
	n := g.nodes[id]
	if n == nil {
		panic("testgateway: unknown node " + id)
	}
	return n
}

// ErrNoSession 表示节点当前没有已认证的会话。
var ErrNoSession = errors.New("testgateway: node has no session")

// bumpLocked 把配置版本加 1 并记录变更（NODE-23、ACS-02），按保留策略裁剪旧变更（NODE-15）。
func (g *Gateway) bumpLocked(n *node, credOnly bool, credIDs []string) uint64 {
	n.version++
	now := g.cfg.Clock.Now()
	n.changes = append(n.changes, change{version: n.version, at: now, credOnly: credOnly, credIDs: credIDs})
	drop := 0
	for drop < len(n.changes) && len(n.changes)-drop > g.cfg.DeltaRetentionVersions && now.Sub(n.changes[drop].at) > g.cfg.DeltaRetention {
		drop++
	}
	n.changes = n.changes[drop:]
	return n.version
}

// UpsertCredentials 新增或替换凭据，版本加 1；节点在线时下发 CredUpsert。
func (g *Gateway) UpsertCredentials(id string, creds ...*nodev1.Credential) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.mustNodeLocked(id)
	ids := make([]string, 0, len(creds))
	for _, c := range creds {
		n.creds[c.GetId()] = c
		ids = append(ids, c.GetId())
	}
	v := g.bumpLocked(n, true, ids)
	g.sendLocked(n, &nodev1.Envelope{Body: &nodev1.Envelope_CredUpsert{CredUpsert: &nodev1.CredUpsert{ConfigVersion: v, Credentials: creds}}})
	g.notifyLocked()
	return v
}

// RemoveCredentials 移除凭据，版本加 1；节点在线时下发 CredRemove。
func (g *Gateway) RemoveCredentials(id string, credIDs ...string) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.mustNodeLocked(id)
	for _, c := range credIDs {
		delete(n.creds, c)
	}
	v := g.bumpLocked(n, true, credIDs)
	g.sendLocked(n, &nodev1.Envelope{Body: &nodev1.Envelope_CredRemove{CredRemove: &nodev1.CredRemove{ConfigVersion: v, CredentialIds: credIDs}}})
	g.notifyLocked()
	return v
}

// SetInbounds 全量替换入站与内核（非凭据变更），版本加 1；节点在线时下发 InboundApply。
// kernel 为 UNSPECIFIED 时保持原内核。
func (g *Gateway) SetInbounds(id string, kernel nodev1.KernelType, inbounds ...*nodev1.Inbound) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.mustNodeLocked(id)
	if kernel != nodev1.KernelType_KERNEL_TYPE_UNSPECIFIED {
		n.kernel = kernel
	}
	n.inbounds = inbounds
	v := g.bumpLocked(n, false, nil)
	g.sendLocked(n, &nodev1.Envelope{Body: &nodev1.Envelope_InboundApply{InboundApply: &nodev1.InboundApply{ConfigVersion: v, Inbounds: inbounds, Kernel: n.kernel}}})
	g.notifyLocked()
	return v
}

// SetRoutes 替换路由与 DNS 服务商凭据（非凭据变更），版本加 1；节点在线时下发 RoutesApply。
func (g *Gateway) SetRoutes(id string, routesJSON, dnsSecret []byte) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.mustNodeLocked(id)
	n.routes, n.dns = routesJSON, dnsSecret
	v := g.bumpLocked(n, false, nil)
	if n.sess != nil {
		ra := &nodev1.RoutesApply{ConfigVersion: v, RoutesJson: routesJSON, DnsProviderSecret: g.sealDNS(n.sess.psk, dnsSecret)}
		g.sendLocked(n, &nodev1.Envelope{Body: &nodev1.Envelope_RoutesApply{RoutesApply: ra}})
	}
	g.notifyLocked()
	return v
}

func (g *Gateway) sealDNS(psk, plain []byte) []byte {
	if len(plain) == 0 {
		return nil
	}
	b, err := nodewire.SealDNSSecret(psk, g.cfg.Rand, plain)
	if err != nil {
		panic(err)
	}
	return b
}

// SendEnvelope 向在线节点发送任意信封（测试用，例如构造过期的指令）；idem_key 为空时生成。
// 不改变控制面的配置版本。
func (g *Gateway) SendEnvelope(id string, env *nodev1.Envelope) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.mustNodeLocked(id)
	if n.sess == nil {
		return ErrNoSession
	}
	g.sendLocked(n, env)
	return nil
}

// SendFull 立即向在线节点下发当前全量快照。
func (g *Gateway) SendFull(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.mustNodeLocked(id)
	if n.sess == nil {
		return ErrNoSession
	}
	g.sendLocked(n, g.fullLocked(n, n.sess.psk))
	n.syncs = append(n.syncs, SyncRecord{At: g.cfg.Clock.Now(), Mode: nodev1.SyncMode_SYNC_MODE_FULL, To: n.version, Reason: "manual"})
	g.notifyLocked()
	return nil
}

// SnapshotEnvelope 返回当前全量快照的信封（测试用，例如篡改校验和后以 SendEnvelope 发送）。
func (g *Gateway) SnapshotEnvelope(id string) *nodev1.Envelope {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.mustNodeLocked(id)
	psk := n.psk
	if n.sess != nil {
		psk = n.sess.psk
	}
	return g.fullLocked(n, psk)
}

// sendLocked 把信封交给节点的当前会话。窗口溢出时丢弃积压，改发一次全量（NODE-14）。
func (g *Gateway) sendLocked(n *node, env *nodev1.Envelope) {
	if n.sess == nil {
		return
	}
	if env.IdemKey == "" {
		env.IdemKey = nodewire.NewUUIDv7(g.cfg.Clock, g.cfg.Rand)
	}
	err := n.sess.s.Send(nodewire.NewOutgoing(env, nil))
	if errors.Is(err, nodewire.ErrWindowFull) {
		n.overflows++
		n.syncs = append(n.syncs, SyncRecord{At: g.cfg.Clock.Now(), Mode: nodev1.SyncMode_SYNC_MODE_FULL, To: n.version, Reason: "overflow"})
		full := nodewire.NewOutgoing(g.fullLocked(n, n.sess.psk), nil)
		full.Exempt = true // 全量快照不计入窗口：之后的增量照常排队，快照再大也不会反复溢出
		_ = n.sess.s.Reset(full)
	}
}

// fullLocked 构造 SyncFull：快照必须能单独重建节点的全部配置状态（20.5）。
func (g *Gateway) fullLocked(n *node, psk []byte) *nodev1.Envelope {
	snap := &nodev1.Snapshot{
		ConfigVersion:     n.version,
		Kernel:            n.kernel,
		Inbounds:          n.inbounds,
		RoutesJson:        n.routes,
		DnsProviderSecret: g.sealDNS(psk, n.dns),
		OfflinePolicy:     n.offline,
	}
	for _, id := range slices.Sorted(maps.Keys(n.creds)) {
		snap.Credentials = append(snap.Credentials, n.creds[id])
	}
	raw, err := nodewire.MarshalDeterministic(snap)
	if err != nil {
		panic(err)
	}
	return &nodev1.Envelope{
		IdemKey: nodewire.NewUUIDv7(g.cfg.Clock, g.cfg.Rand),
		Body:    &nodev1.Envelope_SyncFull{SyncFull: &nodev1.SyncFull{Snapshot: raw, Checksum: nodewire.SnapshotChecksum(raw)}},
	}
}

// syncForLocked 决定重连后的同步方式（NODE-15）：节点落后且中间变更全部是保留期内的凭据变化时发增量，
// 否则发全量；版本相同不发。
func (g *Gateway) syncForLocked(n *node, agentV uint64, psk []byte) (nodev1.SyncMode, *nodev1.Envelope) {
	cur := n.version
	if agentV == cur {
		return nodev1.SyncMode_SYNC_MODE_DELTA, nil
	}
	full := func() (nodev1.SyncMode, *nodev1.Envelope) {
		return nodev1.SyncMode_SYNC_MODE_FULL, g.fullLocked(n, psk)
	}
	if agentV == 0 || agentV > cur || len(n.changes) == 0 || n.changes[0].version > agentV+1 {
		return full()
	}
	changed := make(map[string]bool)
	for _, c := range n.changes {
		if c.version <= agentV {
			continue
		}
		if !c.credOnly {
			return full()
		}
		for _, id := range c.credIDs {
			changed[id] = true
		}
	}
	d := &nodev1.SyncDelta{FromVersion: agentV, ToVersion: cur}
	for _, id := range slices.Sorted(maps.Keys(changed)) {
		if c, ok := n.creds[id]; ok {
			d.Upserts = append(d.Upserts, c)
		} else {
			d.Removals = append(d.Removals, id)
		}
	}
	return nodev1.SyncMode_SYNC_MODE_DELTA, &nodev1.Envelope{
		IdemKey: nodewire.NewUUIDv7(g.cfg.Clock, g.cfg.Rand),
		Body:    &nodev1.Envelope_SyncDelta{SyncDelta: d},
	}
}

// RevokeKey 立即吊销节点密钥（NODE-19）：清空两把 PSK，状态回到 pending_enroll，
// 以 hello_reject(revoked) 与关闭码 4006 关闭会话。重新接入必须签发新的接入令牌。
func (g *Gateway) RevokeKey(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.mustNodeLocked(id)
	n.psk, n.pskPrev, n.revoked = nil, nil, true
	n.tokenHash, n.tokenResp = [32]byte{}, nil
	if n.sess != nil {
		closeWithReject(n.sess, nodev1.HelloRejectReason_HELLO_REJECT_REASON_REVOKED)
	}
	g.notifyLocked()
}

// InjectReject 让该节点接下来的 count 次握手以 reason 被拒绝（在全部校验之前）。
func (g *Gateway) InjectReject(id string, reason nodev1.HelloRejectReason, retryAfterMS uint32, count int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.mustNodeLocked(id)
	for range count {
		n.rejects = append(n.rejects, rejectPlan{reason: reason, retryAfter: retryAfterMS})
	}
}

// ClearRejects 取消尚未生效的注入拒绝。
func (g *Gateway) ClearRejects(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mustNodeLocked(id).rejects = nil
}

// DropSession 立即断开节点的当前连接（模拟网络中断），不发送关闭帧。
func (g *Gateway) DropSession(id string) bool {
	g.mu.Lock()
	n := g.mustNodeLocked(id)
	s := n.sess
	g.mu.Unlock()
	if s == nil {
		return false
	}
	s.s.Abort()
	return true
}

// DropAll 断开全部节点的连接，返回断开的会话数。
func (g *Gateway) DropAll() int {
	g.mu.Lock()
	var ss []*session
	for _, n := range g.nodes {
		if n.sess != nil {
			ss = append(ss, n.sess)
		}
	}
	g.mu.Unlock()
	for _, s := range ss {
		s.s.Abort()
	}
	return len(ss)
}

// PauseReading 暂停或恢复读取该节点的帧：暂停期间不处理确认，控制面方向的待确认条目随之累积。
func (g *Gateway) PauseReading(id string, paused bool) {
	g.mu.Lock()
	gt := g.mustNodeLocked(id).gate
	g.mu.Unlock()
	gt.set(!paused)
}

// SetFaults 设置该节点控制面一侧的帧故障注入；nil 表示停止。
func (g *Gateway) SetFaults(id string, p nodewire.FaultPlan) {
	g.mu.Lock()
	n := g.mustNodeLocked(id)
	g.mu.Unlock()
	n.faults.Set(p)
}

// ExpireIngestKeys 模拟去重键过期（7 天，ACC-03）：之后不大于当前水位的报告确认但不入账。
func (g *Gateway) ExpireIngestKeys(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.mustNodeLocked(id)
	n.baseReportSeq = n.lastReportSeq
	clear(n.ingested)
}

// ingestLocked 按 ACC-03 去重入账：去重键存在即重复；不大于已过期水位的确认但不入账。
func (g *Gateway) ingestLocked(n *node, r *nodev1.ReportTraffic) {
	seq := r.GetReportSeq()
	switch {
	case n.ingested[seq]:
		n.dupReports++
	case seq <= n.baseReportSeq:
		n.staleReports++
	default:
		n.ingested[seq] = true
		n.reportSeqs = append(n.reportSeqs, seq)
		n.lastReportSeq = max(n.lastReportSeq, seq)
		for _, it := range r.GetItems() {
			t := n.totals[it.GetCredentialId()]
			t.Up += it.GetRawUp()
			t.Down += it.GetRawDown()
			n.totals[it.GetCredentialId()] = t
		}
	}
}

// leaseLocked 处理 LeaseRequest（ACC-08、ACC-10），以 (account_id, current_lease_id, is_release) 幂等。
func (g *Gateway) leaseLocked(n *node, r *nodev1.LeaseRequest) {
	n.leaseReqs = append(n.leaseReqs, proto.Clone(r).(*nodev1.LeaseRequest))
	key := fmt.Sprintf("%s|%s|%t", r.GetAccountId(), r.GetCurrentLeaseId(), r.GetIsRelease())
	resp := n.leaseResp[key]
	if resp == nil {
		now := g.cfg.Clock.Now()
		if r.GetIsRelease() {
			if g.cfg.DisableLeaseRelease {
				// 控制面未声明 supports_lease_release，Agent 不应发送释放（ACC-10）。
				n.unexpectedReleases++
				return
			}
			delete(n.leases, r.GetAccountId())
			resp = &nodev1.QuotaLease{AccountId: r.GetAccountId(), Bytes: 0, LeaseId: r.GetCurrentLeaseId()}
		} else {
			gr := grant{id: nodewire.NewUUIDv7(g.cfg.Clock, g.cfg.Rand), bytes: g.cfg.LeaseBytes, expires: now.Add(g.cfg.LeaseTTL)}
			n.leases[r.GetAccountId()] = gr
			resp = &nodev1.QuotaLease{AccountId: r.GetAccountId(), Bytes: gr.bytes, LeaseId: gr.id, ExpiresAtMs: gr.expires.UnixMilli()}
		}
		n.leaseResp[key] = resp
	}
	g.sendLocked(n, &nodev1.Envelope{Body: &nodev1.Envelope_QuotaLease{QuotaLease: resp}})
}
