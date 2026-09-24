// SPDX-License-Identifier: AGPL-3.0-or-later

package testgateway

import (
	"context"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/nodewire"
	"github.com/akari-project/panel/server/internal/logging"
)

// Handler 返回网关的 HTTP 处理器：GET /v1/stream（NODE-05）与 POST /v1/enrollments（NODE-02）。
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/stream", g.serveStream)
	mux.HandleFunc("POST /v1/enrollments", g.serveEnroll)
	return mux
}

// enter 登记一个处理协程；网关已关闭时返回 false。
func (g *Gateway) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ctx.Err() != nil {
		return false
	}
	g.wg.Add(1)
	return true
}

func (g *Gateway) serveStream(w http.ResponseWriter, r *http.Request) {
	if !g.enter() {
		http.Error(w, "closed", http.StatusServiceUnavailable)
		return
	}
	defer g.wg.Done()
	release := func() {}
	if g.pending != nil {
		select {
		case g.pending <- struct{}{}:
			var once sync.Once
			release = func() { once.Do(func() { <-g.pending }) }
		default:
			// 连接准入限速（NODE-07）：面向 Agent 的 HTTP 503，不使用 CONV-16 的错误码。
			w.Header().Set("Retry-After", strconv.Itoa(int(g.cfg.AdmissionRetryAfter.Seconds())))
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
	}
	defer release()
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn := nodewire.WrapWebSocket(ws)
	hctx, cancel := context.WithTimeout(g.ctx, g.cfg.HandshakeTimeout)
	defer cancel()
	b, err := conn.Read(hctx)
	if err != nil {
		// 握手 5 秒内未完成即关闭（20.3 第 5 步）。
		_ = conn.Close(nodewire.ClosePolicy, "handshake timeout")
		return
	}
	f, err := nodewire.UnmarshalFrame(b)
	hello := f.GetHello()
	if err != nil || hello == nil || f.GetSeq() != 0 {
		_ = conn.Close(nodewire.CloseProtocolError, "expected hello")
		return
	}
	res, reject := g.handshake(conn, hello, b)
	release()
	if reject != nil {
		g.cfg.Logger.Info("testgateway: handshake rejected", logging.KeyNodeID, hello.GetNodeId(), "reason", reject.GetReason().String())
		nodewire.SendReject(hctx, conn, reject)
		return
	}
	if res.superseded != nil {
		closeWithReject(res.superseded, nodev1.HelloRejectReason_HELLO_REJECT_REASON_SUPERSEDED)
	}
	if err := conn.Write(hctx, res.ackFrame); err != nil {
		_ = conn.Abort()
	}
	res.sess.s.Start(g.ctx)
	<-res.sess.s.Done()

	g.mu.Lock()
	n := res.node
	if n.sess == res.sess {
		n.sess = nil
	}
	errText := ""
	if err := res.sess.s.Err(); err != nil {
		errText = err.Error()
	}
	n.closes = append(n.closes, CloseRecord{At: g.cfg.Clock.Now(), Gen: res.sess.gen, Err: errText})
	g.notifyLocked()
	g.mu.Unlock()
}

// closeWithReject 以拒绝原因的关闭码关闭已建立的会话（NODE-19 的 4006、NODE-21 的 4007）。
// 会话已认证后只允许 sealed 帧，因此只用关闭码，不发送 hello_reject 帧。
func closeWithReject(s *session, r nodev1.HelloRejectReason) {
	s.s.Close(nodewire.CloseCode(r), r.String())
}

type accepted struct {
	node       *node
	sess       *session
	superseded *session
	ackFrame   []byte
}

func reject(r nodev1.HelloRejectReason, retryAfter uint32) *nodev1.HelloReject {
	return &nodev1.HelloReject{Reason: r, RetryAfterMs: retryAfter, Detail: r.String()}
}

// handshake 按 20.3 校验 Hello 并在同一临界区内登记新会话、排入重连同步，
// 保证之后的配置变更一定发给新会话（NODE-15、NODE-21）。
func (g *Gateway) handshake(conn nodewire.Conn, h *nodev1.Hello, raw []byte) (*accepted, *nodev1.HelloReject) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.cfg.Clock.Now()
	n := g.nodes[h.GetNodeId()]
	rec := Handshake{At: now, Nonce: h.GetNonce(), ConfigVersion: h.GetConfigVersion(), ProtoVersion: h.GetProtoVersion(), Raw: raw}
	fail := func(r nodev1.HelloRejectReason, retryAfter uint32, injected bool) (*accepted, *nodev1.HelloReject) {
		rec.Reject, rec.Injected = r, injected
		if n != nil {
			n.handshakes = append(n.handshakes, rec)
		}
		g.notifyLocked()
		return nil, reject(r, retryAfter)
	}
	if n != nil && len(n.rejects) > 0 {
		p := n.rejects[0]
		n.rejects = n.rejects[1:]
		return fail(p.reason, p.retryAfter, true)
	}
	// NODE-08 时钟偏差。
	if d := now.UnixMilli() - h.GetTsMs(); d > g.cfg.MaxSkew.Milliseconds() || -d > g.cfg.MaxSkew.Milliseconds() {
		return fail(nodev1.HelloRejectReason_HELLO_REJECT_REASON_CLOCK_SKEW, 0, false)
	}
	// NODE-09 防重放：SET NX EX 180 的内存等价物。
	if len(h.GetNonce()) != nodewire.HelloNonceSize {
		return fail(nodev1.HelloRejectReason_HELLO_REJECT_REASON_AUTH_FAILED, 0, false)
	}
	key := hex.EncodeToString(h.GetNonce())
	if exp, ok := g.nonces[key]; ok && now.Before(exp) {
		return fail(nodev1.HelloRejectReason_HELLO_REJECT_REASON_REPLAY, 0, false)
	}
	g.nonces[key] = now.Add(g.cfg.NonceTTL)
	if len(g.nonces)%4096 == 0 {
		for k, exp := range g.nonces {
			if !now.Before(exp) {
				delete(g.nonces, k)
			}
		}
	}
	// NODE-10 MAC；过渡期内新旧两把 PSK 都可以通过。
	if n == nil {
		return fail(nodev1.HelloRejectReason_HELLO_REJECT_REASON_AUTH_FAILED, 0, false)
	}
	if n.psk == nil {
		if n.revoked {
			return fail(nodev1.HelloRejectReason_HELLO_REJECT_REASON_REVOKED, 0, false)
		}
		return fail(nodev1.HelloRejectReason_HELLO_REJECT_REASON_AUTH_FAILED, 0, false)
	}
	psk, prev := n.psk, false
	if !nodewire.VerifyHelloMAC(psk, h) {
		if n.pskPrev == nil || !now.Before(n.transitionUntil) || !nodewire.VerifyHelloMAC(n.pskPrev, h) {
			return fail(nodev1.HelloRejectReason_HELLO_REJECT_REASON_AUTH_FAILED, 0, false)
		}
		psk, prev = n.pskPrev, true
	}
	// 协议版本：低于支持范围拒绝；Agent 更新时按控制面支持的最高版本运行（ENG-03）。
	if h.GetProtoVersion() < g.cfg.ProtoMin {
		return fail(nodev1.HelloRejectReason_HELLO_REJECT_REASON_VERSION_UNSUPPORTED, 0, false)
	}
	selected := min(h.GetProtoVersion(), g.cfg.ProtoMax)
	caps := new(nodev1.Capabilities)
	if err := proto.Unmarshal(h.GetCapabilitiesRaw(), caps); err != nil {
		return fail(nodev1.HelloRejectReason_HELLO_REJECT_REASON_VERSION_UNSUPPORTED, 0, false)
	}

	priv, pub, err := nodewire.GenerateKeyPair(g.cfg.Rand)
	if err != nil {
		return fail(nodev1.HelloRejectReason_HELLO_REJECT_REASON_BUSY, 1000, false)
	}
	up, down, err := nodewire.SessionKeys(priv, h.GetEphemeralPubkey(), h.GetNonce(), h.GetNodeId(), h.GetEphemeralPubkey(), pub)
	if err != nil {
		return fail(nodev1.HelloRejectReason_HELLO_REJECT_REASON_AUTH_FAILED, 0, false)
	}
	mode, initial := g.syncForLocked(n, h.GetConfigVersion(), psk)
	ack := &nodev1.HelloAck{
		EphemeralPubkey:       pub,
		ProtoVersion:          selected,
		SyncMode:              mode,
		ServerCapabilitiesRaw: g.serverCaps,
		LastReportSeq:         n.lastReportSeq,
	}
	ack.Mac = nodewire.HelloAckMAC(psk, h.GetNodeId(), h.GetEphemeralPubkey(), ack)
	ackFrame, err := nodewire.MarshalFrame(&nodev1.Frame{Kind: &nodev1.Frame_HelloAck{HelloAck: ack}})
	if err != nil {
		return fail(nodev1.HelloRejectReason_HELLO_REJECT_REASON_BUSY, 1000, false)
	}

	gs := &session{psk: psk, prevPSK: prev, gen: n.gen + 1}
	sess, err := nodewire.NewSession(nodewire.SessionConfig{
		Conn:    gatedConn{Conn: conn, g: n.gate},
		SendKey: down, RecvKey: up, SendDir: nodewire.DirPanelToAgent,
		Clock: g.cfg.Clock, Rand: g.cfg.Rand,
		WindowMessages: g.cfg.WindowMessages, WindowBytes: g.cfg.WindowBytes,
		Bounded:   true,
		Dedup:     n.dedup,
		Faults:    &n.faults,
		OnReceive: func(*nodev1.Envelope) { g.heartbeat(n) },
		Handler:   func(env *nodev1.Envelope) { g.onEnvelope(n, gs, env) },
	})
	if err != nil {
		return fail(nodev1.HelloRejectReason_HELLO_REJECT_REASON_BUSY, 1000, false)
	}
	gs.s = sess

	// 新 PSK 首次握手成功：清空 psk_prev（NODE-03）。以旧 PSK 建立的会话随即被本会话取代（NODE-21）。
	if !prev && n.pskPrev != nil {
		n.pskPrev = nil
	}
	rec.PrevPSK = prev
	n.handshakes = append(n.handshakes, rec)
	n.caps = caps
	n.gen = gs.gen
	res := &accepted{node: n, sess: gs, superseded: n.sess, ackFrame: ackFrame}
	n.sess = gs
	n.lastSeen = now
	if initial != nil {
		g.sendLocked(n, initial)
	}
	rs := SyncRecord{At: now, Mode: mode, From: h.GetConfigVersion(), To: n.version, Reason: "handshake"}
	if initial == nil {
		rs.Mode = nodev1.SyncMode_SYNC_MODE_UNSPECIFIED // 版本一致，不下发
	}
	n.syncs = append(n.syncs, rs)
	g.notifyLocked()
	return res, nil
}

func (g *Gateway) heartbeat(n *node) {
	g.mu.Lock()
	n.lastSeen = g.cfg.Clock.Now() // NODE-20：任意有效信封都是心跳
	g.mu.Unlock()
}

// onEnvelope 处理节点上报（读协程）。
func (g *Gateway) onEnvelope(n *node, gs *session, env *nodev1.Envelope) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch b := env.GetBody().(type) {
	case *nodev1.Envelope_ReportStatus:
		n.status = b.ReportStatus
		n.statusGen = gs.gen
		n.statuses++
	case *nodev1.Envelope_ReportTraffic:
		g.ingestLocked(n, b.ReportTraffic)
	case *nodev1.Envelope_LeaseRequest:
		if n.sess == gs {
			g.leaseLocked(n, b.LeaseRequest)
		}
	default:
		// 控制面 → 节点方向的消息或未知字段：确认后忽略。
	}
	g.notifyLocked()
}

// gatedConn 在读取每一帧之前等待 gate 打开（PauseReading）。
type gatedConn struct {
	nodewire.Conn
	g *gate
}

func (c gatedConn) Read(ctx context.Context) ([]byte, error) {
	if err := c.g.wait(ctx); err != nil {
		return nil, err
	}
	return c.Conn.Read(ctx)
}

type gate struct {
	mu     sync.Mutex
	ch     chan struct{} // 打开时已关闭
	isOpen bool
}

func newGate() *gate {
	ch := make(chan struct{})
	close(ch)
	return &gate{ch: ch, isOpen: true}
}

func (g *gate) set(open bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case open && !g.isOpen:
		close(g.ch)
	case !open && g.isOpen:
		g.ch = make(chan struct{})
	}
	g.isOpen = open
}

func (g *gate) wait(ctx context.Context) error {
	g.mu.Lock()
	ch := g.ch
	g.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
