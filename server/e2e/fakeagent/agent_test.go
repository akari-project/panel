// SPDX-License-Identifier: AGPL-3.0-or-later

package fakeagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/internal/specdata"
	"github.com/akari-project/panel/server/e2e/nodewire"
	"github.com/akari-project/panel/server/e2e/testgateway"
	"github.com/akari-project/panel/server/internal/clock"
)

// TestHandshakeVectors 让模拟 Agent 以测试向量的输入握手：Hello 的 MAC、HelloAck 的校验、
// 会话密钥与信封加解密都必须与 panel-spec testdata/node-v1-vectors.json 一致（spec/20 20.6）。
func TestHandshakeVectors(t *testing.T) {
	v := specdata.NodeV1Vectors(t)
	in := v.Inputs

	caps := new(nodev1.Capabilities)
	if err := proto.Unmarshal(in.CapabilitiesRaw, caps); err != nil {
		t.Fatal(err)
	}
	type result struct {
		hello *nodev1.Hello
		up    []byte // 解密后的第一个上行信封
		err   error
	}
	got := make(chan result, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			got <- result{err: err}
			return
		}
		defer ws.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		conn := nodewire.WrapWebSocket(ws)
		b, err := conn.Read(ctx)
		if err != nil {
			got <- result{err: err}
			return
		}
		f, _ := nodewire.UnmarshalFrame(b)
		ack := &nodev1.HelloAck{
			EphemeralPubkey:       v.HelloAck.PanelEphemeralPubkey,
			ProtoVersion:          in.ProtoVersion,
			SyncMode:              nodev1.SyncMode(in.SyncMode),
			ServerCapabilitiesRaw: in.ServerCapabilitiesRaw,
			LastReportSeq:         in.LastReportSeq,
			Mac:                   v.HelloAck.MAC,
		}
		ab, _ := nodewire.MarshalFrame(&nodev1.Frame{Kind: &nodev1.Frame_HelloAck{HelloAck: ack}})
		if err := conn.Write(ctx, ab); err != nil {
			got <- result{err: err}
			return
		}
		b, err = conn.Read(ctx)
		if err != nil {
			got <- result{err: err}
			return
		}
		sf, _ := nodewire.UnmarshalFrame(b)
		opener, _ := nodewire.NewOpener(v.Session.KeyAgentToPanel, nodewire.DirAgentToPanel)
		pt, err := opener.Open(sf.GetSeq(), sf.GetSealed())
		// 下行发送向量中的密文（seq 1 的 ack_only），Agent 必须能解开。
		down, _ := nodewire.MarshalFrame(&nodev1.Frame{Kind: &nodev1.Frame_Sealed{Sealed: v.Session.DownSealed}, Seq: v.Session.DownSeq})
		_ = conn.Write(ctx, down)
		got <- result{hello: f.GetHello(), up: pt, err: err}
		for { // 继续读取，以便响应 Agent 停止时的关闭握手
			if _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	// 随机源依次提供：Agent 临时私钥、Hello nonce，之后是真随机数（信封 nonce 与 idem_key）。
	rnd := io.MultiReader(bytes.NewReader(in.AgentEphemeralPrivate), bytes.NewReader(in.HelloNonce), rand.Reader)
	a, err := New(Config{
		NodeID:       in.NodeID,
		PSK:          in.PSK,
		StreamURL:    "ws" + strings.TrimPrefix(srv.URL, "http"),
		Clock:        clock.NewFake(time.UnixMilli(in.TsMs)),
		Rand:         rnd,
		Capabilities: caps,
		State:        &State{ConfigVersion: in.ConfigVersion, Kernel: caps.GetKernel(), NextReportSeq: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a.Start(ctx)
	defer a.Stop()

	var res result
	select {
	case res = <-got:
	case <-ctx.Done():
		t.Fatal("timeout waiting for handshake")
	}
	if res.err != nil {
		t.Fatalf("server: %v", res.err)
	}
	h := res.hello
	if !bytes.Equal(nodewire.HelloMACInput(h), v.Hello.MACInput) {
		t.Fatalf("hello mac_input:\n got  %x\n want %x", nodewire.HelloMACInput(h), v.Hello.MACInput)
	}
	if !bytes.Equal(h.GetMac(), v.Hello.MAC) {
		t.Fatalf("hello mac = %x, want %x", h.GetMac(), v.Hello.MAC)
	}
	env := new(nodev1.Envelope)
	if err := proto.Unmarshal(res.up, env); err != nil || env.GetReportStatus().GetConfigVersion() != in.ConfigVersion {
		t.Fatalf("first up envelope = %v (%v), want ReportStatus{config_version: %d}", env, err, in.ConfigVersion)
	}
	// Agent 接受了 HelloAck（MAC 与密钥派生正确）并解开了下行向量；随后 report_seq 水位来自 HelloAck。
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	defer wcancel()
	if _, ok := a.WaitEvent(wctx, func(e Event) bool { return e.Kind == EventConnected }); !ok {
		t.Fatalf("agent did not accept the hello_ack vector: %+v", a.Events())
	}
	if vw := a.View(); vw.NextReportSeq != in.LastReportSeq+1 {
		t.Fatalf("next report_seq = %d, want %d", vw.NextReportSeq, in.LastReportSeq+1)
	}
	for _, e := range a.Events() {
		if e.Kind == EventDisconnected && strings.Contains(e.Detail, "protocol") {
			t.Fatalf("down vector rejected: %s", e.Detail)
		}
	}
}

// TestAgentAgainstTestGateway 是模拟 Agent 与测试用控制面端的冒烟测试；完整的协议断言在 e2e/conformance。
func TestAgentAgainstTestGateway(t *testing.T) {
	gw := testgateway.Start(t, testgateway.Config{})
	id := gw.CreateNode(testgateway.NodeOptions{})
	token := gw.IssueEnrollToken(id)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	capsRaw, _ := nodewire.MarshalDeterministic(DefaultCapabilities())
	resp, err := Enroll(ctx, gw.Client, gw.URL, &nodev1.EnrollRequest{
		EnrollToken: token, Host: &nodev1.HostInfo{Hostname: "fake"}, CapabilitiesRaw: capsRaw, HostFingerprint: "fp",
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{
		NodeID: resp.GetNodeId(), PSK: resp.GetPsk(), StreamURL: resp.GetStreamUrl(), HTTPClient: gw.Client,
		Timing: Timing{StatusInterval: 100 * time.Millisecond, TrafficInterval: 50 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Start(ctx)
	defer a.Stop()

	gw.SetInbounds(id, 0, &nodev1.Inbound{Tag: "vless-tcp", Protocol: nodev1.Protocol_PROTOCOL_VLESS, ListenPort: 443, Transport: nodev1.Transport_TRANSPORT_TCP})
	v := gw.UpsertCredentials(id, testCred("c1", "acct1"))
	if !gw.WaitNode(ctx, id, func(n testgateway.NodeInfo) bool { return n.State == testgateway.StateOnline && n.AppliedVersion == v }) {
		n, _ := gw.Node(id)
		t.Fatalf("node not online at version %d: %+v", v, n)
	}
	a.AddTraffic("c1", 1000, 2000)
	if !gw.WaitNode(ctx, id, func(n testgateway.NodeInfo) bool { return n.Totals["c1"] == testgateway.Totals{Up: 1000, Down: 2000} }) {
		t.Fatal("traffic not ingested")
	}
	if got := a.View().Credentials; len(got) != 1 || got[0] != "c1" {
		t.Fatalf("credentials = %v", got)
	}
}

func testCred(id, account string) *nodev1.Credential {
	return &nodev1.Credential{
		Id: id, AccountId: account,
		Secret:       bytes.Repeat([]byte{1}, 16),
		Ss2022Key_16: bytes.Repeat([]byte{2}, 16),
		Ss2022Key_32: bytes.Repeat([]byte{3}, 32),
	}
}
