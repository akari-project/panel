// SPDX-License-Identifier: AGPL-3.0-or-later

package nodewire

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"google.golang.org/protobuf/proto"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/internal/specdata"
)

// TestVectors 按 panel-spec testdata/node-v1-vectors.json 逐项核对字节级实现（spec/20 20.6）。
func TestVectors(t *testing.T) {
	v := specdata.NodeV1Vectors(t)
	in := v.Inputs

	hello := &nodev1.Hello{
		NodeId:          in.NodeID,
		TsMs:            in.TsMs,
		Nonce:           in.HelloNonce,
		ProtoVersion:    in.ProtoVersion,
		EphemeralPubkey: v.Hello.AgentEphemeralPubkey,
		ConfigVersion:   in.ConfigVersion,
		CapabilitiesRaw: in.CapabilitiesRaw,
	}
	t.Run("hello", func(t *testing.T) {
		_, pub, err := GenerateKeyPair(bytes.NewReader(in.AgentEphemeralPrivate))
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "agent ephemeral pubkey", pub, v.Hello.AgentEphemeralPubkey)
		eq(t, "hello mac_input", HelloMACInput(hello), v.Hello.MACInput)
		eq(t, "hello mac", HelloMAC(in.PSK, hello), v.Hello.MAC)
		hello.Mac = v.Hello.MAC
		if !VerifyHelloMAC(in.PSK, hello) {
			t.Fatal("VerifyHelloMAC rejected the vector")
		}
		bad := proto.Clone(hello).(*nodev1.Hello)
		bad.ConfigVersion++
		if VerifyHelloMAC(in.PSK, bad) {
			t.Fatal("VerifyHelloMAC accepted a modified hello")
		}
	})

	ack := &nodev1.HelloAck{
		EphemeralPubkey:       v.HelloAck.PanelEphemeralPubkey,
		ProtoVersion:          in.ProtoVersion,
		SyncMode:              nodev1.SyncMode(in.SyncMode),
		ServerCapabilitiesRaw: in.ServerCapabilitiesRaw,
		LastReportSeq:         in.LastReportSeq,
	}
	t.Run("hello_ack", func(t *testing.T) {
		_, pub, err := GenerateKeyPair(bytes.NewReader(in.PanelEphemeralPrivate))
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "panel ephemeral pubkey", pub, v.HelloAck.PanelEphemeralPubkey)
		eq(t, "hello_ack mac_input", HelloAckMACInput(in.NodeID, v.Hello.AgentEphemeralPubkey, ack), v.HelloAck.MACInput)
		eq(t, "hello_ack mac", HelloAckMAC(in.PSK, in.NodeID, v.Hello.AgentEphemeralPubkey, ack), v.HelloAck.MAC)
		ack.Mac = v.HelloAck.MAC
		if !VerifyHelloAckMAC(in.PSK, in.NodeID, v.Hello.AgentEphemeralPubkey, ack) {
			t.Fatal("VerifyHelloAckMAC rejected the vector")
		}
	})

	t.Run("session", func(t *testing.T) {
		s := v.Session
		// 两端各自用本方私钥派生，结果必须与向量一致。
		up, down, err := SessionKeys(in.AgentEphemeralPrivate, v.HelloAck.PanelEphemeralPubkey, in.HelloNonce, in.NodeID, v.Hello.AgentEphemeralPubkey, v.HelloAck.PanelEphemeralPubkey)
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "agent key_agent_to_panel", up, s.KeyAgentToPanel)
		eq(t, "agent key_panel_to_agent", down, s.KeyPanelToAgent)
		up2, down2, err := SessionKeys(in.PanelEphemeralPrivate, v.Hello.AgentEphemeralPubkey, in.HelloNonce, in.NodeID, v.Hello.AgentEphemeralPubkey, v.HelloAck.PanelEphemeralPubkey)
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "panel key_agent_to_panel", up2, s.KeyAgentToPanel)
		eq(t, "panel key_panel_to_agent", down2, s.KeyPanelToAgent)

		upSealer, _ := NewSealer(up, DirAgentToPanel, bytes.NewReader(s.UpNonce))
		got, err := upSealer.Seal(s.UpSeq, s.UpEnvelope)
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "up_sealed", got, s.UpSealed)
		downSealer, _ := NewSealer(down, DirPanelToAgent, bytes.NewReader(s.DownNonce))
		got, err = downSealer.Seal(s.DownSeq, s.DownEnvelope)
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "down_sealed", got, s.DownSealed)

		upOpener, _ := NewOpener(up, DirAgentToPanel)
		pt, err := upOpener.Open(s.UpSeq, s.UpSealed)
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "up_envelope", pt, s.UpEnvelope)
		downOpener, _ := NewOpener(down, DirPanelToAgent)
		pt, err = downOpener.Open(s.DownSeq, s.DownSealed)
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "down_envelope", pt, s.DownEnvelope)

		// 附加数据绑定方向与 seq：换方向或换 seq 都必须失败。
		if _, err := upOpener.Open(s.UpSeq+1, s.UpSealed); err == nil {
			t.Fatal("opened with wrong seq")
		}
		wrongDir, _ := NewOpener(up, DirPanelToAgent)
		if _, err := wrongDir.Open(s.UpSeq, s.UpSealed); err == nil {
			t.Fatal("opened with wrong direction")
		}

		env := new(nodev1.Envelope)
		if err := proto.Unmarshal(s.UpEnvelope, env); err != nil || env.GetReportStatus().GetConfigVersion() != in.ConfigVersion {
			t.Fatalf("up_envelope does not decode as ReportStatus: %v %v", err, env)
		}
	})

	t.Run("sync_full", func(t *testing.T) {
		eq(t, "checksum", SnapshotChecksum(v.SyncFull.Snapshot), v.SyncFull.Checksum)
		if !VerifySnapshot(&nodev1.SyncFull{Snapshot: v.SyncFull.Snapshot, Checksum: v.SyncFull.Checksum}) {
			t.Fatal("VerifySnapshot rejected the vector")
		}
	})

	t.Run("dns_secret", func(t *testing.T) {
		k, err := DNSKey(in.PSK)
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "K_dns", k, v.DNSSecret.Key)
		sealed, err := SealDNSSecret(in.PSK, bytes.NewReader(v.DNSSecret.Nonce), []byte(v.DNSSecret.Plaintext))
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "dns sealed", sealed, v.DNSSecret.Sealed)
		pt, err := OpenDNSSecret(in.PSK, v.DNSSecret.Sealed)
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "dns plaintext", pt, []byte(v.DNSSecret.Plaintext))
	})

	t.Run("agent_upgrade", func(t *testing.T) {
		u := v.AgentUpgrade
		eq(t, "signature_input", AgentUpgradeSignatureInput(u.Version, u.SHA256), u.SignatureInput)
		au := &nodev1.AgentUpgrade{Version: u.Version, Sha256: u.SHA256, Signature: u.Signature, KeyId: u.KeyID}
		if !VerifyAgentUpgrade(map[uint32]ed25519.PublicKey{u.KeyID: ed25519.PublicKey(u.PublicKey)}, au) {
			t.Fatal("VerifyAgentUpgrade rejected the vector")
		}
		au.Version = "0.1.2"
		if VerifyAgentUpgrade(map[uint32]ed25519.PublicKey{u.KeyID: ed25519.PublicKey(u.PublicKey)}, au) {
			t.Fatal("VerifyAgentUpgrade accepted a modified version")
		}
	})

	t.Run("credential", func(t *testing.T) {
		if got := FormatUUID(v.Credential.Secret); got != v.Credential.UUIDText {
			t.Fatalf("uuid_text = %s, want %s", got, v.Credential.UUIDText)
		}
	})
}

func TestParseNodeID(t *testing.T) {
	for _, s := range []string{"0192F000-0000-7000-8000-000000000001", "{0192f000-0000-7000-8000-000000000001}", "0192f0000000700080000000000000001", ""} {
		if _, err := ParseNodeID(s); err == nil {
			t.Errorf("ParseNodeID(%q) accepted", s)
		}
	}
}

func TestCloseCodes(t *testing.T) {
	for r := nodev1.HelloRejectReason_HELLO_REJECT_REASON_CLOCK_SKEW; r <= nodev1.HelloRejectReason_HELLO_REJECT_REASON_SUPERSEDED; r++ {
		got, ok := ReasonFromCloseCode(CloseCode(r))
		if !ok || got != r {
			t.Errorf("%s: close code %d round trip = %s %v", r, CloseCode(r), got, ok)
		}
	}
	for _, c := range []int{1000, 4000, 4008} {
		if _, ok := ReasonFromCloseCode(c); ok {
			t.Errorf("close code %d mapped to a reason", c)
		}
	}
}

func eq(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s:\n got  %x\n want %x", what, got, want)
	}
}
