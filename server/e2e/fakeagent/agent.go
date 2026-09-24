// SPDX-License-Identifier: AGPL-3.0-or-later

package fakeagent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/nodewire"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/logging"
)

// State 是模拟 Agent 的本地状态（spec/21 AGT-05）：重启时原样交给新实例。
// 真实 Agent 把这些内容加密保存在状态目录中；模拟 Agent 只保存在内存中。
type State struct {
	ConfigVersion     uint64
	Snapshot          []byte // 最后一次应用的快照原始字节
	Kernel            nodev1.KernelType
	Inbounds          []*nodev1.Inbound
	Credentials       map[string]*nodev1.Credential
	RoutesJSON        []byte
	DNSProviderSecret []byte
	OfflinePolicy     *nodev1.OfflinePolicy

	// NextReportSeq 是下一份流量报告的 report_seq，跨重启持续递增（spec/22 ACC-03）。
	NextReportSeq uint64
	// ReportSeqKnown 表示已从 HelloAck.last_report_seq 取得过水位；新装的 Agent 在此之前不生成报告。
	ReportSeqKnown bool
	// WAL 是尚未确认的流量报告（按 report_seq），重连与重启后先重传（ACC-02）。
	WAL map[uint64]*nodev1.Envelope

	Dedup  *nodewire.Dedup // 收到的 idem_key（NODE-13）
	Leases map[string]*Lease
}

// Lease 是某账号在本节点的配额租约（spec/22 ACC-08）。
type Lease struct {
	ID        string
	Granted   int64
	Remaining int64
	ExpiresAt time.Time
	renewing  bool
}

func (s *State) clone() *State {
	c := *s
	c.Inbounds = slices.Clone(s.Inbounds)
	c.Credentials = cloneMap(s.Credentials)
	c.WAL = cloneMap(s.WAL)
	c.Leases = make(map[string]*Lease, len(s.Leases))
	for k, v := range s.Leases {
		l := *v
		c.Leases[k] = &l
	}
	return &c
}

func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// 发送队列中条目的标签。
type (
	reportTag uint64
	statusTag struct{}
)

// Agent 是一个模拟节点。
type Agent struct {
	cfg     Config
	timing  Timing
	log     *slog.Logger
	clock   clock.Clock
	rand    io.Reader
	kernels map[nodev1.KernelType]*nodev1.KernelSupport
	events  *eventLog
	started time.Time

	jmu    sync.Mutex
	jitter *mrand.Rand

	cmu      sync.Mutex
	counters map[string]*counter

	mu           sync.Mutex
	st           *State
	sess         *nodewire.Session
	carry        []*nodewire.Outgoing // 离线期间或会话结束后未发出的条目（租约请求）
	handshakes   int
	serverCaps   *nodev1.ControlPlaneCapabilities
	wantFull     bool
	fullAttempts int
	applyErr     string
	credErrs     map[string]struct{}
	conns        map[string]int
	leaseWait    map[string]time.Time
	applied      []Applied
	upgrades     []*nodev1.AgentUpgrade
	sourceSets   map[string]*nodev1.SourceSet

	cancel context.CancelFunc
	done   chan struct{}
}

// Applied 记录一条被应用的带版本指令。
type Applied struct {
	Body    string
	Version uint64
	IdemKey string
}

// New 创建模拟 Agent；调用 Start 后开始连接。
func New(cfg Config) (*Agent, error) {
	if _, err := nodewire.ParseNodeID(cfg.NodeID); err != nil {
		return nil, err
	}
	if len(cfg.PSK) != nodewire.PSKSize {
		return nil, errors.New("fakeagent: PSK must be 32 bytes")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	} else if cfg.Rand != rand.Reader {
		cfg.Rand = nodewire.LockedReader(cfg.Rand) // 连接循环、会话写协程与测试调用方并发读取
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Capabilities == nil {
		cfg.Capabilities = DefaultCapabilities()
	}
	if cfg.ProtoVersion == 0 {
		cfg.ProtoVersion = nodewire.ProtoVersion
	}
	a := &Agent{
		cfg:        cfg,
		timing:     cfg.Timing.withDefaults(),
		log:        cfg.Logger.With(logging.KeyNodeID, cfg.NodeID),
		clock:      cfg.Clock,
		rand:       cfg.Rand,
		kernels:    make(map[nodev1.KernelType]*nodev1.KernelSupport),
		events:     newEventLog(),
		jitter:     mrand.New(mrand.NewPCG(cfg.Seed, cfg.Seed^0x6a09e667f3bcc909)),
		counters:   make(map[string]*counter),
		credErrs:   make(map[string]struct{}),
		conns:      make(map[string]int),
		leaseWait:  make(map[string]time.Time),
		sourceSets: make(map[string]*nodev1.SourceSet),
		done:       make(chan struct{}),
	}
	a.started = a.clock.Now()
	for _, k := range cfg.Capabilities.GetKernels() {
		a.kernels[k.GetKernel()] = k
	}
	if cfg.State != nil {
		a.st = cfg.State.clone()
	} else {
		a.st = &State{Kernel: cfg.Capabilities.GetKernel(), NextReportSeq: 1}
	}
	if a.st.Credentials == nil {
		a.st.Credentials = make(map[string]*nodev1.Credential)
	}
	if a.st.WAL == nil {
		a.st.WAL = make(map[uint64]*nodev1.Envelope)
	}
	if a.st.Leases == nil {
		a.st.Leases = make(map[string]*Lease)
	}
	if a.st.Dedup == nil {
		a.st.Dedup = nodewire.NewDedup(a.clock, 0)
	}
	return a, nil
}

// NodeID 返回节点 ID。
func (a *Agent) NodeID() string { return a.cfg.NodeID }

// Start 在后台运行连接循环，直到 Stop 或 ctx 结束。
func (a *Agent) Start(ctx context.Context) {
	ctx, a.cancel = context.WithCancel(ctx)
	go a.run(ctx)
}

// Stop 关闭会话（关闭码 1001）并等待连接循环退出。
func (a *Agent) Stop() {
	if a.cancel != nil {
		a.cancel()
		<-a.done
	}
}

// Done 在连接循环退出后关闭。
func (a *Agent) Done() <-chan struct{} { return a.done }

// PersistedState 返回本地状态的副本，用于以同一状态启动新实例（模拟重启，AGT-05）。
// 应在 Stop 之后调用。
func (a *Agent) PersistedState() *State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st.clone()
}

// Disconnect 立即断开当前连接（模拟网络中断），之后按 NODE-07 重连。
func (a *Agent) Disconnect() {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		s.Abort()
	}
}

func (a *Agent) run(ctx context.Context) {
	defer close(a.done)
	status := time.NewTicker(a.timing.StatusInterval)
	traffic := time.NewTicker(a.timing.TrafficInterval)
	defer status.Stop()
	defer traffic.Stop()
	failures := 0
	for ctx.Err() == nil {
		sess, err := a.connect(ctx)
		if err == nil {
			failures = 0
			err = a.serve(ctx, sess, status.C, traffic.C)
		}
		if ctx.Err() != nil {
			return
		}
		delay := a.retryDelay(err, &failures)
		t := time.NewTimer(delay)
	wait:
		for {
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				break wait
			case <-traffic.C:
				a.FlushTraffic()
			case <-status.C:
				a.housekeeping()
			}
		}
	}
}

// retryDelay 按结束原因决定重连前的等待（NODE-07、NODE-22），并记录事件。
func (a *Agent) retryDelay(err error, failures *int) time.Duration {
	a.mu.Lock()
	wantFull := a.wantFull
	if wantFull {
		a.fullAttempts++
	}
	fullAttempts := a.fullAttempts
	a.mu.Unlock()

	if re, ok := nodewire.RejectFromError(err); ok {
		var d time.Duration
		switch re.Reason {
		case nodev1.HelloRejectReason_HELLO_REJECT_REASON_REPLAY:
			d = 0 // 生成新的 nonce 后立即重试
		case nodev1.HelloRejectReason_HELLO_REJECT_REASON_BUSY:
			if re.RetryAfterMS > 0 {
				d = time.Duration(re.RetryAfterMS) * time.Millisecond // 按控制面给出的时间等待，不缩放
			} else {
				d = a.backoff(*failures)
				*failures++
			}
		case nodev1.HelloRejectReason_HELLO_REJECT_REASON_AUTH_FAILED:
			d = a.expBackoff(*failures, time.Hour)
			*failures++
		case nodev1.HelloRejectReason_HELLO_REJECT_REASON_VERSION_UNSUPPORTED,
			nodev1.HelloRejectReason_HELLO_REJECT_REASON_REVOKED,
			nodev1.HelloRejectReason_HELLO_REJECT_REASON_SUPERSEDED:
			// 本进程每个节点只有一条会话，superseded 说明另有 Agent 以同一身份运行（NODE-22）。
			d = a.scale(time.Hour)
		default: // clock_skew 与未知原因按 NODE-07 退避
			d = a.backoff(*failures)
			*failures++
		}
		a.log.Warn("fakeagent: rejected", "reason", re.Reason.String(), "retry_in", d)
		a.events.add(Event{Kind: EventRejected, At: a.clock.Now(), Reason: re.Reason, Delay: d, Detail: re.Detail})
		return d
	}
	var d time.Duration
	switch {
	case wantFull && fullAttempts <= 1:
		d = 0 // 请求全量同步：立即以 config_version=0 重连
	case wantFull:
		d = a.backoff(fullAttempts - 2)
	default:
		d = a.backoff(*failures)
		*failures++
	}
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	a.events.add(Event{Kind: EventDisconnected, At: a.clock.Now(), Delay: d, Detail: detail})
	return d
}

func (a *Agent) scale(d time.Duration) time.Duration {
	return time.Duration(float64(d) * a.timing.BackoffScale)
}

// backoff 返回 random(0, min(30s, 1s × 2ⁿ))（NODE-07）。
func (a *Agent) backoff(n int) time.Duration {
	limit := 30 * time.Second
	if n < 5 {
		limit = time.Second << n
	}
	a.jmu.Lock()
	d := time.Duration(a.jitter.Int64N(int64(limit)))
	a.jmu.Unlock()
	return a.scale(d)
}

// expBackoff 返回 [½, 1] × min(max, 1s × 2ⁿ)（auth_failed，上限 1 小时）。
func (a *Agent) expBackoff(n int, max time.Duration) time.Duration {
	limit := max
	if n < 12 && time.Second<<n < max {
		limit = time.Second << n
	}
	a.jmu.Lock()
	d := limit/2 + time.Duration(a.jitter.Int64N(int64(limit/2)+1))
	a.jmu.Unlock()
	return a.scale(d)
}

// connect 建立连接并完成握手（20.3），成功时返回已启动的会话。
func (a *Agent) connect(ctx context.Context) (*nodewire.Session, error) {
	a.events.add(Event{Kind: EventDial, At: a.clock.Now()})
	hctx, cancel := context.WithTimeout(ctx, a.timing.HandshakeTimeout)
	defer cancel()

	ws, resp, err := websocket.Dial(hctx, a.cfg.StreamURL, &websocket.DialOptions{HTTPClient: a.cfg.HTTPClient})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusServiceUnavailable {
			// 网关连接准入限速（NODE-07）：按 Retry-After 等待，与 busy 相同。
			secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			return nil, &nodewire.RejectError{Reason: nodev1.HelloRejectReason_HELLO_REJECT_REASON_BUSY, RetryAfterMS: uint32(secs * 1000), Detail: "admission 503"}
		}
		return nil, fmt.Errorf("fakeagent: dial: %w", err)
	}
	conn := nodewire.WrapWebSocket(ws)

	priv, pub, err := nodewire.GenerateKeyPair(a.rand)
	if err != nil {
		_ = conn.Abort()
		return nil, err
	}
	nonce := make([]byte, nodewire.HelloNonceSize)
	if _, err := io.ReadFull(a.rand, nonce); err != nil {
		_ = conn.Abort()
		return nil, err
	}
	a.mu.Lock()
	cv := a.st.ConfigVersion
	if a.wantFull {
		cv = 0
	}
	caps := proto.Clone(a.cfg.Capabilities).(*nodev1.Capabilities)
	if k := a.kernels[a.st.Kernel]; k != nil {
		caps.Kernel, caps.KernelVersion = k.GetKernel(), k.GetVersion()
	}
	a.mu.Unlock()
	capsRaw, err := nodewire.MarshalDeterministic(caps)
	if err != nil {
		_ = conn.Abort()
		return nil, err
	}
	hello := &nodev1.Hello{
		NodeId:          a.cfg.NodeID,
		TsMs:            a.clock.Now().UnixMilli(),
		Nonce:           nonce,
		ProtoVersion:    a.cfg.ProtoVersion,
		EphemeralPubkey: pub,
		ConfigVersion:   cv,
		CapabilitiesRaw: capsRaw,
	}
	hello.Mac = nodewire.HelloMAC(a.cfg.PSK, hello)
	b, err := nodewire.MarshalFrame(&nodev1.Frame{Kind: &nodev1.Frame_Hello{Hello: hello}})
	if err != nil {
		_ = conn.Abort()
		return nil, err
	}
	if err := conn.Write(hctx, b); err != nil {
		_ = conn.Abort()
		return nil, fmt.Errorf("fakeagent: write hello: %w", err)
	}
	b, err = conn.Read(hctx)
	if err != nil {
		_ = conn.Abort()
		if re, ok := nodewire.RejectFromError(err); ok {
			return nil, re
		}
		return nil, fmt.Errorf("fakeagent: read hello_ack: %w", err)
	}
	f, err := nodewire.UnmarshalFrame(b)
	if err != nil {
		_ = conn.Abort()
		return nil, err
	}
	if r := f.GetHelloReject(); r != nil {
		_ = conn.Abort()
		return nil, &nodewire.RejectError{Reason: r.GetReason(), RetryAfterMS: r.GetRetryAfterMs(), Detail: r.GetDetail()}
	}
	ack := f.GetHelloAck()
	if ack == nil || f.GetSeq() != 0 {
		_ = conn.Close(nodewire.CloseProtocolError, "expected hello_ack")
		return nil, fmt.Errorf("%w: expected hello_ack", nodewire.ErrProtocol)
	}
	if !nodewire.VerifyHelloAckMAC(a.cfg.PSK, a.cfg.NodeID, pub, ack) {
		_ = conn.Close(nodewire.ClosePolicy, "hello_ack mac")
		return nil, fmt.Errorf("%w: hello_ack mac mismatch", nodewire.ErrProtocol)
	}
	if ack.GetProtoVersion() != a.cfg.ProtoVersion {
		_ = conn.Close(nodewire.ClosePolicy, "proto version")
		return nil, fmt.Errorf("%w: control plane selected proto_version %d", nodewire.ErrProtocol, ack.GetProtoVersion())
	}
	up, down, err := nodewire.SessionKeys(priv, ack.GetEphemeralPubkey(), nonce, a.cfg.NodeID, pub, ack.GetEphemeralPubkey())
	if err != nil {
		_ = conn.Close(nodewire.ClosePolicy, "key agreement")
		return nil, err
	}
	serverCaps := new(nodev1.ControlPlaneCapabilities)
	if err := proto.Unmarshal(ack.GetServerCapabilitiesRaw(), serverCaps); err != nil {
		_ = conn.Close(nodewire.ClosePolicy, "server capabilities")
		return nil, fmt.Errorf("%w: server capabilities: %v", nodewire.ErrProtocol, err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if last := ack.GetLastReportSeq(); a.st.NextReportSeq <= last {
		a.st.NextReportSeq = last + 1 // 新装或重装：从已入账水位之后开始（ACC-03）
	}
	a.st.ReportSeqKnown = true
	a.serverCaps = serverCaps
	a.handshakes++

	// 重传顺序：WAL 中的流量报告（按 report_seq），然后是上一会话未确认的其他条目（NODE-12）。
	var initial []*nodewire.Outgoing
	for _, seq := range slices.Sorted(mapKeys(a.st.WAL)) {
		initial = append(initial, nodewire.NewOutgoing(a.st.WAL[seq], reportTag(seq)))
	}
	initial = append(initial, a.carry...)
	a.carry = nil

	var sess *nodewire.Session
	sess, err = nodewire.NewSession(nodewire.SessionConfig{
		Conn: conn, SendKey: up, RecvKey: down, SendDir: nodewire.DirAgentToPanel,
		Clock: a.clock, Rand: a.rand,
		WindowMessages: a.timing.WindowMessages, WindowBytes: a.timing.WindowBytes,
		Dedup:   a.st.Dedup,
		Faults:  a.cfg.Faults,
		Handler: func(env *nodev1.Envelope) { a.onEnvelope(sess, env) },
		OnAcked: a.onAcked,
		Initial: initial,
	})
	if err != nil {
		_ = conn.Abort()
		return nil, err
	}
	// 会话不随 Agent 的 ctx 取消：Stop 时由 serve 以 1001 关闭（coder/websocket 在 ctx 取消时直接断开连接）。
	sess.Start(context.WithoutCancel(ctx))
	a.sess = sess
	a.log.Info("fakeagent: connected", "sync_mode", ack.GetSyncMode().String(), "config_version", cv)
	a.events.add(Event{Kind: EventConnected, At: a.clock.Now(), Mode: ack.GetSyncMode(), Version: cv})
	a.sendStatusLocked()
	return sess, nil
}

func mapKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// serve 在会话存续期间处理定时上报与保活，返回会话结束的原因。
func (a *Agent) serve(ctx context.Context, sess *nodewire.Session, status, traffic <-chan time.Time) error {
	ping := time.NewTicker(a.timing.PingInterval)
	defer ping.Stop()
	misses := 0
	defer func() {
		a.mu.Lock()
		for _, o := range sess.Unacked() {
			switch o.Tag.(type) {
			case reportTag, statusTag: // 报告从 WAL 重传；状态重连后重新上报
			default:
				a.carryLocked(o)
			}
		}
		if a.sess == sess {
			a.sess = nil
		}
		a.mu.Unlock()
	}()
	for {
		select {
		case <-sess.Done():
			return sess.Err()
		case <-ctx.Done():
			sess.Close(nodewire.CloseGoingAway, "agent stopping")
			<-sess.Done()
			return ctx.Err()
		case <-status:
			a.housekeeping()
			a.mu.Lock()
			a.sendStatusLocked()
			a.mu.Unlock()
		case <-traffic:
			a.FlushTraffic()
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, a.timing.PingInterval)
			err := sess.Ping(pctx)
			cancel()
			if err == nil {
				misses = 0
			} else if misses++; misses >= a.timing.PingMisses {
				a.log.Warn("fakeagent: ping timeout, reconnecting")
				sess.Abort()
			}
		}
	}
}

// sendLocked 把条目交给当前会话；离线或会话已结束时暂存，下次连接后发送。调用方持有 a.mu。
// 暂存的条目按 Key 合并（同一账号只保留最新的租约请求），流量报告另由 WAL 保存。
func (a *Agent) sendLocked(o *nodewire.Outgoing) {
	if a.sess != nil && a.sess.Send(o) == nil {
		return
	}
	switch o.Tag.(type) {
	case reportTag, statusTag:
	default:
		a.carryLocked(o)
	}
}

func (a *Agent) carryLocked(o *nodewire.Outgoing) {
	if o.Key != "" {
		for i, c := range a.carry {
			if c.Key == o.Key {
				a.carry[i] = o
				return
			}
		}
	}
	a.carry = append(a.carry, o)
}

func (a *Agent) newEnvelope() *nodev1.Envelope {
	return &nodev1.Envelope{IdemKey: nodewire.NewUUIDv7(a.clock, a.rand)}
}

func (a *Agent) onAcked(acked []*nodewire.Outgoing) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, o := range acked {
		if seq, ok := o.Tag.(reportTag); ok {
			if _, inWAL := a.st.WAL[uint64(seq)]; inWAL {
				delete(a.st.WAL, uint64(seq))
				a.events.add(Event{Kind: EventReportAcked, At: a.clock.Now(), Seq: uint64(seq)})
			}
		}
	}
}
