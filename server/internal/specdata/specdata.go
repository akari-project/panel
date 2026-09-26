// SPDX-License-Identifier: AGPL-3.0-or-later

// Package specdata 读取 panel-spec 模块中的测试数据（testdata/node-v1-vectors.json）。
//
// 测试向量是契约的一部分，不复制到本仓库：从 go.mod 依赖的 panel-spec 版本所在目录读取
// （go list -m 解析，go.work 联调时即本地的 panel-spec）。设置 PANEL_SPEC_DIR 可以指定其他目录。
package specdata

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const module = "github.com/akari-project/panel-spec"

var (
	dirOnce sync.Once
	dir     string
	dirErr  error
)

// Dir 返回 panel-spec 模块的根目录。
func Dir(t testing.TB) string {
	t.Helper()
	dirOnce.Do(func() {
		if d := os.Getenv("PANEL_SPEC_DIR"); d != "" {
			dir = d
			return
		}
		out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", module).Output()
		if err != nil {
			dirErr = err
			return
		}
		dir = strings.TrimSpace(string(out))
	})
	if dirErr != nil || dir == "" {
		t.Fatalf("specdata: cannot locate %s (set PANEL_SPEC_DIR): %v", module, dirErr)
	}
	return dir
}

// Hex 是 JSON 中以十六进制表示的字节串。
type Hex []byte

// UnmarshalJSON 实现 json.Unmarshaler。
func (h *Hex) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := hex.DecodeString(s)
	*h = v
	return err
}

// Vectors 是 node-v1-vectors.json 中本仓库用到的部分。
type Vectors struct {
	Inputs struct {
		PSK                   Hex    `json:"psk"`
		NodeID                string `json:"node_id"`
		TsMs                  int64  `json:"ts_ms"`
		HelloNonce            Hex    `json:"hello_nonce"`
		ProtoVersion          uint32 `json:"proto_version"`
		ConfigVersion         uint64 `json:"config_version"`
		LastReportSeq         uint64 `json:"last_report_seq"`
		SyncMode              int32  `json:"sync_mode"`
		AgentEphemeralPrivate Hex    `json:"agent_ephemeral_private"`
		PanelEphemeralPrivate Hex    `json:"panel_ephemeral_private"`
		CapabilitiesRaw       Hex    `json:"capabilities_raw"`
		ServerCapabilitiesRaw Hex    `json:"server_capabilities_raw"`
	} `json:"inputs"`
	Hello struct {
		AgentEphemeralPubkey Hex `json:"agent_ephemeral_pubkey"`
		MACInput             Hex `json:"mac_input"`
		MAC                  Hex `json:"mac"`
	} `json:"hello"`
	HelloAck struct {
		PanelEphemeralPubkey Hex `json:"panel_ephemeral_pubkey"`
		MACInput             Hex `json:"mac_input"`
		MAC                  Hex `json:"mac"`
	} `json:"hello_ack"`
	Session struct {
		SharedSecret    Hex    `json:"shared_secret"`
		HKDFInfo        Hex    `json:"hkdf_info"`
		KeyAgentToPanel Hex    `json:"key_agent_to_panel"`
		KeyPanelToAgent Hex    `json:"key_panel_to_agent"`
		UpSeq           uint64 `json:"up_seq"`
		UpNonce         Hex    `json:"up_nonce"`
		UpEnvelope      Hex    `json:"up_envelope"`
		UpSealed        Hex    `json:"up_sealed"`
		DownSeq         uint64 `json:"down_seq"`
		DownNonce       Hex    `json:"down_nonce"`
		DownEnvelope    Hex    `json:"down_envelope"`
		DownSealed      Hex    `json:"down_sealed"`
	} `json:"session"`
	DNSSecret struct {
		Key       Hex    `json:"key"`
		Plaintext string `json:"plaintext"`
		Nonce     Hex    `json:"nonce"`
		Sealed    Hex    `json:"sealed"`
	} `json:"dns_secret"`
	AgentUpgrade struct {
		KeyID          uint32 `json:"key_id"`
		PublicKey      Hex    `json:"public_key"`
		Version        string `json:"version"`
		SHA256         Hex    `json:"sha256"`
		SignatureInput Hex    `json:"signature_input"`
		Signature      Hex    `json:"signature"`
	} `json:"agent_upgrade"`
	Credential struct {
		Secret      Hex    `json:"secret"`
		UUIDText    string `json:"uuid_text"`
		SS2022Key16 Hex    `json:"ss2022_key_16"`
		SS2022Key32 Hex    `json:"ss2022_key_32"`
	} `json:"credential"`
	SyncFull struct {
		Snapshot Hex `json:"snapshot"`
		Checksum Hex `json:"checksum"`
	} `json:"sync_full"`
}

// NodeV1Vectors 读取并解析 testdata/node-v1-vectors.json。
func NodeV1Vectors(t testing.TB) *Vectors {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(Dir(t), "testdata", "node-v1-vectors.json"))
	if err != nil {
		t.Fatalf("specdata: %v", err)
	}
	v := new(Vectors)
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("specdata: node-v1-vectors.json: %v", err)
	}
	return v
}
