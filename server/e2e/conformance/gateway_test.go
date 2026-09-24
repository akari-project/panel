// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

// 网关断言：以原始协议客户端检查控制面一侧的行为（20.3、NODE-02、NODE-03、NODE-12、NODE-15、NODE-21、ACC-03）。
// 目前对 e2e/testgateway 运行，用于固定参照实现的行为；M2-02 实现 internal/gateway 后，
// 这些用例改为对真实 gateway 运行（启动 panel gateway 与 PostgreSQL、Valkey，替换 newGatewayEnv），
// 断言本身不变。以 TestAgent_ 开头的用例不属于此类。

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/fakeagent"
	"github.com/akari-project/panel/server/e2e/nodewire"
	"github.com/akari-project/panel/server/e2e/testgateway"
	"github.com/akari-project/panel/server/internal/clock"
)

// rawSession 是手工驱动的协议客户端。
type rawSession struct {
	t      *testing.T
	ws     *websocket.Conn
	conn   nodewire.Conn
	hello  []byte // 发送的 hello 帧原始字节
	ack    *nodev1.HelloAck
	reject *nodev1.HelloReject
	sealer *nodewire.Sealer
	opener *nodewire.Opener
	seq    uint64
	recv   uint64
}

func rawDial(t *testing.T, gw *testgateway.Server) *rawSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, gw.StreamURL, &websocket.DialOptions{HTTPClient: gw.Client})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &rawSession{t: t, ws: ws, conn: nodewire.WrapWebSocket(ws)}
}

// newHello 构造一个合法的 Hello；mutate 在计算 MAC 之前调用，mutateAfter 在之后调用。
func newHello(t *testing.T, clk clock.Clock, nodeID string, psk []byte, mutate, mutateAfter func(*nodev1.Hello)) (*nodev1.Hello, []byte) {
	t.Helper()
	priv, pub, err := nodewire.GenerateKeyPair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caps, _ := nodewire.MarshalDeterministic(fakeagent.DefaultCapabilities())
	h := &nodev1.Hello{
		NodeId: nodeID, TsMs: clk.Now().UnixMilli(), Nonce: random(16), ProtoVersion: nodewire.ProtoVersion,
		EphemeralPubkey: pub, CapabilitiesRaw: caps,
	}
	if mutate != nil {
		mutate(h)
	}
	h.Mac = nodewire.HelloMAC(psk, h)
	if mutateAfter != nil {
		mutateAfter(h)
	}
	return h, priv
}

// handshake 发送 hello 帧并读取回应。
func (r *rawSession) handshake(frame []byte, h *nodev1.Hello, priv, psk []byte) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.hello = frame
	if err := r.conn.Write(ctx, frame); err != nil {
		r.t.Fatalf("write hello: %v", err)
	}
	b, err := r.conn.Read(ctx)
	if err != nil {
		r.t.Fatalf("read hello response: %v", err)
	}
	f, err := nodewire.UnmarshalFrame(b)
	if err != nil {
		r.t.Fatal(err)
	}
	if r.reject = f.GetHelloReject(); r.reject != nil {
		return
	}
	r.ack = f.GetHelloAck()
	if r.ack == nil || f.GetSeq() != 0 {
		r.t.Fatalf("unexpected response frame %v", f)
	}
	if !nodewire.VerifyHelloAckMAC(psk, h.GetNodeId(), h.GetEphemeralPubkey(), r.ack) {
		r.t.Fatal("hello_ack MAC does not verify with the PSK that authenticated the hello (NODE-10)")
	}
	up, down, err := nodewire.SessionKeys(priv, r.ack.GetEphemeralPubkey(), h.GetNonce(), h.GetNodeId(), h.GetEphemeralPubkey(), r.ack.GetEphemeralPubkey())
	if err != nil {
		r.t.Fatal(err)
	}
	r.sealer, _ = nodewire.NewSealer(up, nodewire.DirAgentToPanel, rand.Reader)
	r.opener, _ = nodewire.NewOpener(down, nodewire.DirPanelToAgent)
}

func rawHandshake(t *testing.T, gw *testgateway.Server, clk clock.Clock, nodeID string, psk []byte, mutate, mutateAfter func(*nodev1.Hello)) *rawSession {
	t.Helper()
	r := rawDial(t, gw)
	h, priv := newHello(t, clk, nodeID, psk, mutate, mutateAfter)
	frame, _ := nodewire.MarshalFrame(&nodev1.Frame{Kind: &nodev1.Frame_Hello{Hello: h}})
	r.handshake(frame, h, priv, psk)
	return r
}

// expectClosed 等待连接关闭并返回关闭码。
func (r *rawSession) expectClosed(within time.Duration) int {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	for {
		b, err := r.conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				r.t.Fatalf("connection still open after %v", within)
			}
			return nodewire.CloseCodeOf(err)
		}
		if f, _ := nodewire.UnmarshalFrame(b); f.GetHelloReject() != nil {
			continue
		}
	}
}

func (r *rawSession) expectReject(want nodev1.HelloRejectReason) {
	r.t.Helper()
	if r.reject == nil {
		r.t.Fatalf("handshake accepted, want hello_reject(%s)", want)
	}
	if r.reject.GetReason() != want {
		r.t.Fatalf("hello_reject(%s), want %s", r.reject.GetReason(), want)
	}
	if code := r.expectClosed(5 * time.Second); code != nodewire.CloseCode(want) {
		r.t.Fatalf("close code %d, want %d (NODE-22)", code, nodewire.CloseCode(want))
	}
}

func (r *rawSession) expectAccepted() {
	r.t.Helper()
	if r.reject != nil {
		r.t.Fatalf("handshake rejected: %s", r.reject.GetReason())
	}
}

// send 以指定 seq 发送一个信封（seq 为 0 时取下一个）。
func (r *rawSession) send(seq uint64, env *nodev1.Envelope) {
	r.t.Helper()
	if seq == 0 {
		r.seq++
		seq = r.seq
	}
	if env.IdemKey == "" {
		env.IdemKey = nodewire.NewUUIDv7(clock.Real{}, rand.Reader)
	}
	pt, _ := proto.Marshal(env)
	sealed, err := r.sealer.Seal(seq, pt)
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := nodewire.MarshalFrame(&nodev1.Frame{Kind: &nodev1.Frame_Sealed{Sealed: sealed}, Seq: seq})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.conn.Write(ctx, b); err != nil {
		r.t.Fatalf("write: %v", err)
	}
}

// next 读取下一个信封。
func (r *rawSession) next() *nodev1.Envelope {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := r.conn.Read(ctx)
	if err != nil {
		r.t.Fatalf("read: %v", err)
	}
	f, err := nodewire.UnmarshalFrame(b)
	if err != nil {
		r.t.Fatal(err)
	}
	if f.GetSeq() != r.recv+1 {
		r.t.Fatalf("control plane seq %d, want %d (NODE-12)", f.GetSeq(), r.recv+1)
	}
	r.recv = f.GetSeq()
	pt, err := r.opener.Open(f.GetSeq(), f.GetSealed())
	if err != nil {
		r.t.Fatalf("open: %v", err)
	}
	env := new(nodev1.Envelope)
	if err := proto.Unmarshal(pt, env); err != nil {
		r.t.Fatal(err)
	}
	return env
}

// nextBody 读取信封直到遇到满足 match 的业务消息。
func (r *rawSession) nextBody(match func(*nodev1.Envelope) bool) *nodev1.Envelope {
	r.t.Helper()
	for range 50 {
		if env := r.next(); match(env) {
			return env
		}
	}
	r.t.Fatal("expected message not received")
	return nil
}

type gatewayEnv struct {
	gw  *testgateway.Server
	clk *offsetClock
}

func newGatewayEnv(t *testing.T, cfg testgateway.Config) *gatewayEnv {
	clk := &offsetClock{}
	if cfg.Clock == nil {
		cfg.Clock = clk
	}
	return &gatewayEnv{gw: testgateway.Start(t, cfg), clk: clk}
}

func (g *gatewayEnv) provisioned() (string, []byte) {
	id := g.gw.CreateNode(testgateway.NodeOptions{})
	return id, g.gw.Provision(id)
}

func TestGateway_HandshakeChecks(t *testing.T) {
	t.Parallel()
	g := newGatewayEnv(t, testgateway.Config{})
	id, psk := g.provisioned()
	skew := func(d time.Duration) func(*nodev1.Hello) {
		return func(h *nodev1.Hello) { h.TsMs += d.Milliseconds() }
	}
	t.Run("accepted", func(t *testing.T) {
		r := rawHandshake(t, g.gw, g.clk, id, psk, nil, nil)
		r.expectAccepted()
		if r.ack.GetProtoVersion() != nodewire.ProtoVersion {
			t.Errorf("selected proto_version %d", r.ack.GetProtoVersion())
		}
	})
	t.Run("clock_skew", func(t *testing.T) { // NODE-08
		rawHandshake(t, g.gw, g.clk, id, psk, skew(-61*time.Second), nil).expectReject(nodev1.HelloRejectReason_HELLO_REJECT_REASON_CLOCK_SKEW)
		rawHandshake(t, g.gw, g.clk, id, psk, skew(61*time.Second), nil).expectReject(nodev1.HelloRejectReason_HELLO_REJECT_REASON_CLOCK_SKEW)
		rawHandshake(t, g.gw, g.clk, id, psk, skew(-55*time.Second), nil).expectAccepted()
	})
	t.Run("auth_failed", func(t *testing.T) { // NODE-10
		rawHandshake(t, g.gw, g.clk, id, random(32), nil, nil).expectReject(nodev1.HelloRejectReason_HELLO_REJECT_REASON_AUTH_FAILED)
		rawHandshake(t, g.gw, g.clk, "0192f000-0000-7000-8000-00000000dead", psk, nil, nil).expectReject(nodev1.HelloRejectReason_HELLO_REJECT_REASON_AUTH_FAILED)
		// MAC 覆盖 config_version 与能力的原始字节：计算 MAC 之后再改动即失败。
		rawHandshake(t, g.gw, g.clk, id, psk, nil, func(h *nodev1.Hello) { h.ConfigVersion = 7 }).expectReject(nodev1.HelloRejectReason_HELLO_REJECT_REASON_AUTH_FAILED)
		rawHandshake(t, g.gw, g.clk, id, psk, nil, func(h *nodev1.Hello) { h.CapabilitiesRaw = append(h.CapabilitiesRaw, 0x50, 0x01) }).expectReject(nodev1.HelloRejectReason_HELLO_REJECT_REASON_AUTH_FAILED)
	})
	t.Run("unknown_capability_fields", func(t *testing.T) {
		// 新版 Agent 的能力带旧网关不认识的字段：MAC 按原始字节计算，握手成功（20.3 第 1 步）。
		rawHandshake(t, g.gw, g.clk, id, psk, func(h *nodev1.Hello) { h.CapabilitiesRaw = append(h.CapabilitiesRaw, 0xf8, 0x07, 0x01) }, nil).expectAccepted()
	})
	t.Run("version", func(t *testing.T) {
		rawHandshake(t, g.gw, g.clk, id, psk, func(h *nodev1.Hello) { h.ProtoVersion = 0 }, nil).expectReject(nodev1.HelloRejectReason_HELLO_REJECT_REASON_VERSION_UNSUPPORTED)
		// Agent 比控制面新：控制面选定自己支持的版本（ENG-03）。
		r := rawHandshake(t, g.gw, g.clk, id, psk, func(h *nodev1.Hello) { h.ProtoVersion = nodewire.ProtoVersion + 1 }, nil)
		r.expectAccepted()
		if r.ack.GetProtoVersion() != nodewire.ProtoVersion {
			t.Errorf("selected proto_version %d, want %d", r.ack.GetProtoVersion(), nodewire.ProtoVersion)
		}
	})
	t.Run("replay", func(t *testing.T) { // NODE-09
		r := rawHandshake(t, g.gw, g.clk, id, psk, nil, nil)
		r.expectAccepted()
		replay := rawDial(t, g.gw)
		h := new(nodev1.Frame)
		_ = proto.Unmarshal(r.hello, h)
		replay.handshake(r.hello, h.GetHello(), nil, psk)
		replay.expectReject(nodev1.HelloRejectReason_HELLO_REJECT_REASON_REPLAY)
	})
}

// TestGateway_HandshakeTimeout：5 秒内没有完成握手即关闭（20.3 第 5 步；参照实现缩短为 300 ms）。
func TestGateway_HandshakeTimeout(t *testing.T) {
	t.Parallel()
	g := newGatewayEnv(t, testgateway.Config{HandshakeTimeout: 300 * time.Millisecond})
	r := rawDial(t, g.gw)
	real := clock.Real{}
	start := real.Now()
	r.expectClosed(5 * time.Second)
	if d := real.Now().Sub(start); d > 3*time.Second {
		t.Fatalf("closed after %v", d)
	}
}

// TestGateway_SeqViolations：会话内重复、跳号的 seq 与认证失败的密文都关闭连接（NODE-12）。
func TestGateway_SeqViolations(t *testing.T) {
	t.Parallel()
	g := newGatewayEnv(t, testgateway.Config{})
	id, psk := g.provisioned()
	status := &nodev1.Envelope{Body: &nodev1.Envelope_ReportStatus{ReportStatus: &nodev1.ReportStatus{}}}
	for name, seqs := range map[string][]uint64{"duplicate": {1, 1}, "gap": {1, 3}, "zero": {0}} {
		t.Run(name, func(t *testing.T) {
			r := rawHandshake(t, g.gw, g.clk, id, psk, nil, nil)
			r.expectAccepted()
			for _, s := range seqs {
				if s == 0 {
					r.seq = ^uint64(0) // 下一个 seq 回绕为 0
				}
				r.send(s, proto.Clone(status).(*nodev1.Envelope))
			}
			if code := r.expectClosed(5 * time.Second); code != nodewire.CloseProtocolError {
				t.Fatalf("close code %d, want %d", code, nodewire.CloseProtocolError)
			}
		})
	}
	t.Run("bad_ciphertext", func(t *testing.T) {
		r := rawHandshake(t, g.gw, g.clk, id, psk, nil, nil)
		r.expectAccepted()
		r.sealer, _ = nodewire.NewSealer(random(32), nodewire.DirAgentToPanel, rand.Reader)
		r.send(0, proto.Clone(status).(*nodev1.Envelope))
		r.expectClosed(5 * time.Second)
	})
}

// TestGateway_SupersedeAndTransition：同一节点至多一条会话，新会话取代旧会话（4007，NODE-21）；
// 重新接入的过渡期内新旧 PSK 都能通过，新 PSK 首次握手后旧 PSK 失效（NODE-03、NODE-10）。
func TestGateway_SupersedeAndTransition(t *testing.T) {
	t.Parallel()
	g := newGatewayEnv(t, testgateway.Config{})
	id := g.gw.CreateNode(testgateway.NodeOptions{})
	enroll := func(fp string) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		caps, _ := nodewire.MarshalDeterministic(fakeagent.DefaultCapabilities())
		resp, err := fakeagent.Enroll(ctx, g.gw.Client, g.gw.URL, &nodev1.EnrollRequest{EnrollToken: g.gw.IssueEnrollToken(id), CapabilitiesRaw: caps, HostFingerprint: fp})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetPsk()
	}
	psk1 := enroll("host-1")
	a := rawHandshake(t, g.gw, g.clk, id, psk1, nil, nil)
	a.expectAccepted()
	b := rawHandshake(t, g.gw, g.clk, id, psk1, nil, nil)
	b.expectAccepted()
	if code := a.expectClosed(5 * time.Second); code != nodewire.CloseCode(nodev1.HelloRejectReason_HELLO_REJECT_REASON_SUPERSEDED) {
		t.Fatalf("superseded session closed with %d, want 4007", code)
	}

	psk2 := enroll("host-2")
	if n, _ := g.gw.Node(id); !n.HasPrevPSK {
		t.Fatal("re-enrollment did not keep psk_prev")
	}
	old := rawHandshake(t, g.gw, g.clk, id, psk1, nil, nil) // 过渡期：旧 PSK 仍可通过
	old.expectAccepted()
	fresh := rawHandshake(t, g.gw, g.clk, id, psk2, nil, nil)
	fresh.expectAccepted()
	old.expectClosed(5 * time.Second)
	if n, _ := g.gw.Node(id); n.HasPrevPSK {
		t.Fatal("psk_prev not cleared after the new PSK's first handshake (NODE-03)")
	}
	rawHandshake(t, g.gw, g.clk, id, psk1, nil, nil).expectReject(nodev1.HelloRejectReason_HELLO_REJECT_REASON_AUTH_FAILED)

	// 立即吊销（NODE-19）：会话以 4006 关闭，之后的握手为 revoked。
	g.gw.RevokeKey(id)
	if code := fresh.expectClosed(5 * time.Second); code != nodewire.CloseCode(nodev1.HelloRejectReason_HELLO_REJECT_REASON_REVOKED) {
		t.Fatalf("revoked session closed with %d, want 4006", code)
	}
	rawHandshake(t, g.gw, g.clk, id, psk2, nil, nil).expectReject(nodev1.HelloRejectReason_HELLO_REJECT_REASON_REVOKED)
}

// TestGateway_Enrollment：接入令牌一次有效；首次成功后 10 分钟内同一指纹重试返回相同结果，其他情况 404（NODE-02）。
func TestGateway_Enrollment(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	g := newGatewayEnv(t, testgateway.Config{Clock: clk})
	id := g.gw.CreateNode(testgateway.NodeOptions{})
	token := g.gw.IssueEnrollToken(id)
	caps, _ := nodewire.MarshalDeterministic(fakeagent.DefaultCapabilities())
	post := func(tok, fp string, raw []byte) (int, []byte) {
		body := raw
		if body == nil {
			body, _ = proto.Marshal(&nodev1.EnrollRequest{EnrollToken: tok, CapabilitiesRaw: caps, HostFingerprint: fp})
		}
		resp, err := g.gw.Client.Post(g.gw.URL+"/v1/enrollments", "application/x-protobuf", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK && resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Errorf("error content type %q, want problem+json (NODE-18)", resp.Header.Get("Content-Type"))
		}
		return resp.StatusCode, b
	}
	code, first := post(token, "fp-a", nil)
	if code != http.StatusOK {
		t.Fatalf("enroll: %d %s", code, first)
	}
	resp := new(nodev1.EnrollResponse)
	if err := proto.Unmarshal(first, resp); err != nil || resp.GetNodeId() != id || len(resp.GetPsk()) != 32 {
		t.Fatalf("response %v %v", resp, err)
	}
	if code, again := post(token, "fp-a", nil); code != http.StatusOK || !bytes.Equal(again, first) {
		t.Fatalf("retry with the same fingerprint: %d, identical=%v", code, bytes.Equal(again, first))
	}
	if code, _ := post(token, "fp-b", nil); code != http.StatusNotFound {
		t.Fatalf("other fingerprint: %d, want 404", code)
	}
	clk.Advance(11 * time.Minute)
	if code, _ := post(token, "fp-a", nil); code != http.StatusNotFound {
		t.Fatalf("retry after 10 minutes: %d, want 404", code)
	}
	if code, _ := post("no-such-token", "fp-a", nil); code != http.StatusNotFound {
		t.Fatalf("unknown token: %d", code)
	}
	expired := g.gw.IssueEnrollToken(id)
	clk.Advance(24*time.Hour + time.Second)
	if code, _ := post(expired, "fp-a", nil); code != http.StatusNotFound {
		t.Fatalf("expired token: %d, want 404", code)
	}
	if code, _ := post("", "", []byte{0xff, 0xff}); code != http.StatusBadRequest {
		t.Fatalf("malformed body: %d, want 400", code)
	}
}

// TestGateway_Admission：并发握手超过上限时返回 HTTP 503 与 Retry-After（NODE-07）。
func TestGateway_Admission(t *testing.T) {
	t.Parallel()
	g := newGatewayEnv(t, testgateway.Config{MaxHandshakesPending: 1, HandshakeTimeout: 3 * time.Second})
	rawDial(t, g.gw) // 占住名额：不发送 hello
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var resp *http.Response
	var err error
	for {
		_, resp, err = websocket.Dial(ctx, g.gw.StreamURL, &websocket.DialOptions{HTTPClient: g.gw.Client})
		if err != nil || ctx.Err() != nil {
			break
		}
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("second handshake: %v %v, want 503 with Retry-After", resp, err)
	}
}

// TestGateway_ReconnectSyncChoice：控制面按 NODE-15 选择同步方式：落后且只有保留期内的凭据变化时发增量；
// 超出保留（这里只保留 2 个版本）或含非凭据变更时发全量。
func TestGateway_ReconnectSyncChoice(t *testing.T) {
	t.Parallel()
	g := newGatewayEnv(t, testgateway.Config{DeltaRetentionVersions: 2, DeltaRetention: time.Nanosecond})
	id, psk := g.provisioned()
	g.gw.SetInbounds(id, 0, vlessTCP("in"))
	v := g.gw.UpsertCredentials(id, cred("c1", "a"))
	helloAt := func(cv uint64) *rawSession {
		r := rawHandshake(t, g.gw, g.clk, id, psk, func(h *nodev1.Hello) { h.ConfigVersion = cv }, nil)
		r.expectAccepted()
		return r
	}
	if r := helloAt(v); r.ack.GetSyncMode() != nodev1.SyncMode_SYNC_MODE_DELTA {
		t.Errorf("up to date: sync_mode %s", r.ack.GetSyncMode())
	}
	g.gw.UpsertCredentials(id, cred("c2", "a"))
	v3 := g.gw.RemoveCredentials(id, "c1")
	r := helloAt(v)
	if r.ack.GetSyncMode() != nodev1.SyncMode_SYNC_MODE_DELTA {
		t.Fatalf("credential-only changes: sync_mode %s", r.ack.GetSyncMode())
	}
	d := r.nextBody(func(e *nodev1.Envelope) bool { return e.GetSyncDelta() != nil }).GetSyncDelta()
	if d.GetFromVersion() != v || d.GetToVersion() != v3 || len(d.GetUpserts()) != 1 || d.GetUpserts()[0].GetId() != "c2" || len(d.GetRemovals()) != 1 || d.GetRemovals()[0] != "c1" {
		t.Errorf("delta = %v", d)
	}
	g.gw.UpsertCredentials(id, cred("c3", "a"))
	if r := helloAt(v); r.ack.GetSyncMode() != nodev1.SyncMode_SYNC_MODE_FULL {
		t.Errorf("beyond retention: sync_mode %s, want full", r.ack.GetSyncMode())
	}
	cur, _ := g.gw.Node(id)
	g.gw.SetRoutes(id, []byte(`{"rules":[]}`), []byte(`{"provider":"cloudflare"}`))
	r = helloAt(cur.Version)
	if r.ack.GetSyncMode() != nodev1.SyncMode_SYNC_MODE_FULL {
		t.Fatalf("non-credential change: sync_mode %s, want full", r.ack.GetSyncMode())
	}
	sf := r.nextBody(func(e *nodev1.Envelope) bool { return e.GetSyncFull() != nil }).GetSyncFull()
	if !nodewire.VerifySnapshot(sf) {
		t.Fatal("snapshot checksum does not cover the raw snapshot bytes (NODE-15)")
	}
	snap := new(nodev1.Snapshot)
	_ = proto.Unmarshal(sf.GetSnapshot(), snap)
	// DNS 服务商凭据以当前会话 PSK 派生的 K_dns 加密（NODE-25）。
	if pt, err := nodewire.OpenDNSSecret(psk, snap.GetDnsProviderSecret()); err != nil || string(pt) != `{"provider":"cloudflare"}` {
		t.Fatalf("dns secret: %q %v", pt, err)
	}
	if snap.GetOfflinePolicy().GetOfflineLeaseBytes() != 64<<20 || snap.GetOfflinePolicy().GetOfflineMaxSeconds() != 86400 {
		t.Errorf("offline policy = %v (ACC-19 defaults)", snap.GetOfflinePolicy())
	}
	if r := helloAt(0); r.ack.GetSyncMode() != nodev1.SyncMode_SYNC_MODE_FULL {
		t.Errorf("config_version 0: sync_mode %s, want full (NODE-23)", r.ack.GetSyncMode())
	}
}

// TestGateway_IngestDedup：report_seq 去重，重复报告确认但不重复入账；去重键过期后不大于水位的报告
// 确认但不入账（ACC-03）；idem_key 相同的信封只处理一次（NODE-13）；HelloAck 下发已入账水位。
func TestGateway_IngestDedup(t *testing.T) {
	t.Parallel()
	g := newGatewayEnv(t, testgateway.Config{})
	id := g.gw.CreateNode(testgateway.NodeOptions{LastReportSeq: 40})
	psk := g.gw.Provision(id)
	r := rawHandshake(t, g.gw, g.clk, id, psk, nil, nil)
	r.expectAccepted()
	if r.ack.GetLastReportSeq() != 40 {
		t.Fatalf("hello_ack last_report_seq %d, want 40", r.ack.GetLastReportSeq())
	}
	report := func(seq uint64, idem string) *nodev1.Envelope {
		return &nodev1.Envelope{IdemKey: idem, Body: &nodev1.Envelope_ReportTraffic{ReportTraffic: &nodev1.ReportTraffic{
			ReportSeq: seq, Items: []*nodev1.TrafficItem{{CredentialId: "c", RawUp: 100, RawDown: 1}},
		}}}
	}
	r.send(0, report(41, ""))
	r.send(0, report(41, "")) // 同一 report_seq、不同 idem_key：按 report_seq 去重
	same := nodewire.NewUUIDv7(clock.Real{}, rand.Reader)
	r.send(0, report(42, same))
	r.send(0, report(43, same)) // 同一 idem_key：信封级去重
	r.send(0, report(30, ""))   // 不大于初始水位
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !g.gw.WaitNode(ctx, id, func(n testgateway.NodeInfo) bool { return n.DupReports+n.StaleReports+len(n.ReportSeqs) >= 4 }) {
		t.Fatal("reports not processed")
	}
	n, _ := g.gw.Node(id)
	if n.Totals["c"].Up != 200 || n.DupReports != 1 || n.StaleReports != 1 || n.LastReportSeq != 42 {
		t.Fatalf("totals %v dup %d stale %d last %d", n.Totals, n.DupReports, n.StaleReports, n.LastReportSeq)
	}
	// 控制面确认收到的信封（ack 为已收到的最大连续 seq）。
	env := r.nextBody(func(e *nodev1.Envelope) bool { return e.GetAck() >= r.seq })
	if env.GetAck() != r.seq {
		t.Errorf("ack %d, want %d", env.GetAck(), r.seq)
	}
	g.gw.ExpireIngestKeys(id)
	r.send(0, report(42, ""))
	r.send(0, report(44, ""))
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if !g.gw.WaitNode(ctx2, id, func(n testgateway.NodeInfo) bool { return n.LastReportSeq == 44 }) {
		t.Fatal("report 44 not ingested")
	}
	n, _ = g.gw.Node(id)
	if n.StaleReports != 2 || n.Totals["c"].Up != 300 {
		t.Fatalf("after key expiry: stale %d totals %v", n.StaleReports, n.Totals)
	}
	r2 := rawHandshake(t, g.gw, g.clk, id, psk, nil, nil)
	r2.expectAccepted()
	if r2.ack.GetLastReportSeq() != 44 {
		t.Fatalf("hello_ack last_report_seq %d, want 44", r2.ack.GetLastReportSeq())
	}
}

// TestGateway_LeaseIdempotency：LeaseRequest 以 (account_id, current_lease_id, is_release) 幂等；
// 释放回复 QuotaLease{bytes = 0}（ACC-08、ACC-10）。
func TestGateway_LeaseIdempotency(t *testing.T) {
	t.Parallel()
	g := newGatewayEnv(t, testgateway.Config{})
	id, psk := g.provisioned()
	r := rawHandshake(t, g.gw, g.clk, id, psk, nil, nil)
	r.expectAccepted()
	caps := new(nodev1.ControlPlaneCapabilities)
	if err := proto.Unmarshal(r.ack.GetServerCapabilitiesRaw(), caps); err != nil || !caps.GetSupportsLeaseRelease() {
		t.Fatalf("server capabilities %v %v", caps, err)
	}
	lease := func(req *nodev1.LeaseRequest) *nodev1.QuotaLease {
		r.send(0, &nodev1.Envelope{Body: &nodev1.Envelope_LeaseRequest{LeaseRequest: req}})
		return r.nextBody(func(e *nodev1.Envelope) bool { return e.GetQuotaLease() != nil }).GetQuotaLease()
	}
	a := lease(&nodev1.LeaseRequest{AccountId: "acct"})
	b := lease(&nodev1.LeaseRequest{AccountId: "acct"})
	if a.GetLeaseId() == "" || a.GetBytes() <= 0 || a.GetLeaseId() != b.GetLeaseId() {
		t.Fatalf("initial requests not idempotent: %v / %v", a, b)
	}
	c := lease(&nodev1.LeaseRequest{AccountId: "acct", CurrentLeaseId: a.GetLeaseId(), RemainingBytes: 10})
	if c.GetLeaseId() == a.GetLeaseId() {
		t.Fatal("renewal returned the old lease")
	}
	rel := lease(&nodev1.LeaseRequest{AccountId: "acct", CurrentLeaseId: c.GetLeaseId(), IsRelease: true})
	if rel.GetBytes() != 0 || rel.GetLeaseId() != c.GetLeaseId() {
		t.Fatalf("release reply = %v, want bytes 0 for %s", rel, c.GetLeaseId())
	}
}
