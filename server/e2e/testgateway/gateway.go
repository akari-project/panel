// SPDX-License-Identifier: AGPL-3.0-or-later

// Package testgateway 是测试用的最小控制面端：严格按 spec/20 实现节点协议的控制面一侧
// （接入、握手校验、会话、可靠投递、版本同步、窗口溢出转全量、流量入账去重、租约），
// 状态全部在内存中，不连接 PostgreSQL 与 Valkey。
//
// 用途：在 internal/gateway（M2-02）实现之前，作为 e2e/conformance 一致性套件中 Agent 的对端；
// 也作为 gateway 行为断言的参照实现。M2-02 之后，conformance 中标注为“网关断言”的用例改为对
// 真实 gateway 运行，本包只保留作 Agent 一致性测试的对端（见 e2e/conformance/README.md）。
//
// 与真实 gateway 的差异：nonce 防重放（NODE-09）用内存表代替 Valkey SET NX；入账（ACC-03）
// 只做 report_seq 去重与按凭据累加，不做倍率、归属与超额判定；租约按固定额度发放。
package testgateway

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"sync"
	"time"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/nodewire"
	"github.com/akari-project/panel/server/internal/clock"
)

// Config 配置测试用控制面端，零值字段取规格默认值。
type Config struct {
	Clock  clock.Clock
	Rand   io.Reader
	Logger *slog.Logger

	MaxSkew          time.Duration // NODE-08，默认 60 秒
	NonceTTL         time.Duration // NODE-09，默认 180 秒
	HandshakeTimeout time.Duration // 默认 5 秒
	ProtoMin         uint32        // 默认 nodewire.ProtoVersion
	ProtoMax         uint32

	WindowMessages int // NODE-14，默认 1,000
	WindowBytes    int // 默认 8 MiB

	// 凭据变更的保留：最近 DeltaRetention 时间或最近 DeltaRetentionVersions 个版本（NODE-15）。
	DeltaRetention         time.Duration // 默认 24 小时
	DeltaRetentionVersions int           // 默认 10,000

	LeaseBytes           int64         // 每次发放的额度，默认 64 MiB
	LeaseTTL             time.Duration // 默认 10 分钟（ACC-08）
	DisableLeaseRelease  bool          // 不声明 supports_lease_release
	TransitionPeriod     time.Duration // 重新接入的过渡期上限，默认 24 小时（NODE-10）
	EnrollTokenTTL       time.Duration // 默认 24 小时（NODE-01）
	EnrollRetryWindow    time.Duration // 默认 10 分钟（NODE-02）
	OfflineWindow        time.Duration // 心跳超时判为 offline，默认 90 秒（NODE-20）
	MaxHandshakesPending int           // 并发握手上限，超过返回 HTTP 503（NODE-07 准入）；0 表示不限
	AdmissionRetryAfter  time.Duration // 503 的 Retry-After，默认 1 秒
	PanelVersion         string
}

func (c Config) withDefaults() Config {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	if c.Clock == nil {
		c.Clock = clock.Real{}
	}
	if c.Rand == nil {
		c.Rand = rand.Reader
	} else if c.Rand != rand.Reader {
		c.Rand = nodewire.LockedReader(c.Rand) // 各节点的会话写协程并发读取
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	def(&c.MaxSkew, 60*time.Second)
	def(&c.NonceTTL, 180*time.Second)
	def(&c.HandshakeTimeout, 5*time.Second)
	if c.ProtoMin == 0 {
		c.ProtoMin = nodewire.ProtoVersion
	}
	if c.ProtoMax == 0 {
		c.ProtoMax = nodewire.ProtoVersion
	}
	if c.WindowMessages <= 0 {
		c.WindowMessages = nodewire.DefaultWindowMessages
	}
	if c.WindowBytes <= 0 {
		c.WindowBytes = nodewire.DefaultWindowBytes
	}
	def(&c.DeltaRetention, 24*time.Hour)
	if c.DeltaRetentionVersions <= 0 {
		c.DeltaRetentionVersions = 10000
	}
	if c.LeaseBytes <= 0 {
		c.LeaseBytes = 64 << 20
	}
	def(&c.LeaseTTL, 10*time.Minute)
	def(&c.TransitionPeriod, 24*time.Hour)
	def(&c.EnrollTokenTTL, 24*time.Hour)
	def(&c.EnrollRetryWindow, 10*time.Minute)
	def(&c.OfflineWindow, 90*time.Second)
	def(&c.AdmissionRetryAfter, time.Second)
	if c.PanelVersion == "" {
		c.PanelVersion = "0.0.0-testgateway"
	}
	return c
}

// Gateway 是测试用控制面端。全部状态由 mu 保护；持锁期间不做 I/O（Session.Send 只入队）。
type Gateway struct {
	cfg        Config
	serverCaps []byte
	ctx        context.Context
	cancel     context.CancelFunc
	pending    chan struct{} // 并发握手的信号量；nil 表示不限

	mu      sync.Mutex
	nodes   map[string]*node
	nonces  map[string]time.Time
	changed chan struct{}
	wg      sync.WaitGroup
}

// New 创建测试用控制面端。
func New(cfg Config) *Gateway {
	cfg = cfg.withDefaults()
	caps, err := nodewire.MarshalDeterministic(&nodev1.ControlPlaneCapabilities{
		PanelVersion:         cfg.PanelVersion,
		SupportsLeaseRelease: !cfg.DisableLeaseRelease,
	})
	if err != nil {
		panic(err)
	}
	g := &Gateway{
		cfg:        cfg,
		serverCaps: caps,
		nodes:      make(map[string]*node),
		nonces:     make(map[string]time.Time),
		changed:    make(chan struct{}),
	}
	if cfg.MaxHandshakesPending > 0 {
		g.pending = make(chan struct{}, cfg.MaxHandshakesPending)
	}
	g.ctx, g.cancel = context.WithCancel(context.Background())
	return g
}

// Close 中断全部会话并等待处理协程退出。
func (g *Gateway) Close() {
	g.mu.Lock()
	g.cancel()
	var ss []*session
	for _, n := range g.nodes {
		if n.sess != nil {
			ss = append(ss, n.sess)
		}
	}
	g.mu.Unlock()
	for _, s := range ss { // 解锁后再关闭连接：持锁期间不做 I/O
		s.s.Abort()
	}
	g.wg.Wait()
}

// notifyLocked 唤醒全部等待者。调用方持有 g.mu。
func (g *Gateway) notifyLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// waitPoll 是 Wait 在没有状态变更通知时重新检查条件的间隔。
const waitPoll = 500 * time.Millisecond

// Wait 等待 cond 成立；cond 在持锁状态下以 Gateway 的只读视图调用。ctx 结束时返回 false。
func (g *Gateway) Wait(ctx context.Context, cond func(v View) bool) bool {
	for {
		g.mu.Lock()
		ok := cond(View{g})
		ch := g.changed
		g.mu.Unlock()
		if ok {
			return true
		}
		// 节点状态中的 offline 由时钟推进产生，不会触发通知，因此另以固定间隔重新检查。
		t := time.NewTimer(waitPoll)
		select {
		case <-ch:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return false
		}
		t.Stop()
	}
}

// WaitNode 等待某节点的状态满足 cond。
func (g *Gateway) WaitNode(ctx context.Context, id string, cond func(NodeInfo) bool) bool {
	return g.Wait(ctx, func(v View) bool {
		n, ok := v.Node(id)
		return ok && cond(n)
	})
}

// View 是 Wait 回调中的只读视图，只能在回调内使用。
type View struct{ g *Gateway }

// Node 返回节点信息。
func (v View) Node(id string) (NodeInfo, bool) {
	n := v.g.nodes[id]
	if n == nil {
		return NodeInfo{}, false
	}
	return v.g.infoLocked(n), true
}

// Nodes 返回全部节点 ID。
func (v View) Nodes() []string {
	ids := make([]string, 0, len(v.g.nodes))
	for id := range v.g.nodes {
		ids = append(ids, id)
	}
	return ids
}

// Count 返回满足 cond 的节点数（不复制节点信息，适合大量节点）。
func (v View) Count(cond func(Summary) bool) int {
	c := 0
	for _, n := range v.g.nodes {
		if cond(v.g.summaryLocked(n)) {
			c++
		}
	}
	return c
}
