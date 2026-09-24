// SPDX-License-Identifier: AGPL-3.0-or-later

// Package nodewire 实现节点协议（spec/20，panel-spec proto/node/v1）的字节级定义：
// 握手 MAC、会话密钥派生、信封加密、快照校验和与 DNS 服务商凭据加密。
//
// 字节布局以 panel-spec 的 envelope.proto 与 messages.proto 注释为准，并由
// testdata/node-v1-vectors.json 的测试向量验证（spec/20 20.6）。模拟 Agent（e2e/fakeagent）与
// 测试用控制面端（e2e/testgateway）共用本包；本包不含任何会话状态。
package nodewire

import (
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
	"google.golang.org/protobuf/proto"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"
)

// ProtoVersion 是本实现支持的节点协议版本。
const ProtoVersion uint32 = 1

// 方向字节（附加数据的第一个字节，NODE-11）。
const (
	DirAgentToPanel byte = 0x01
	DirPanelToAgent byte = 0x02
)

// 长度常量。
const (
	HelloNonceSize = 16                          // Hello.nonce
	KeySize        = 32                          // X25519 公私钥、会话密钥、PSK
	SealNonceSize  = chacha20poly1305.NonceSizeX // Frame.sealed 的 nonce
	SealOverhead   = SealNonceSize + chacha20poly1305.Overhead
	PSKSize        = 32
)

// 各消息 MAC 与密钥派生使用的域分隔字符串。
const (
	helloLabel    = "akari-node-hello-v1"
	helloAckLabel = "akari-node-hello-ack-v1"
	sessionInfo   = "akari-node-session-v1"
	dnsInfo       = "akari-dns-secret-v1"
)

// ErrOpen 表示密文认证失败。
var ErrOpen = errors.New("nodewire: message authentication failed")

// field 按“字符串与字节串前置 4 字节大端长度”编码（envelope.proto 文件头）。
func field(b []byte) []byte {
	out := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	return append(out, b...)
}

func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// Sum256 返回 SHA-256 摘要。
func Sum256(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// HelloMACInput 返回 Hello.mac 的输入 M（envelope.proto Hello.mac）。
// 能力按收到的原始字节 capabilities_raw 取摘要，不重新序列化。
func HelloMACInput(h *nodev1.Hello) []byte {
	return concat(
		field([]byte(helloLabel)),
		field([]byte(h.GetNodeId())),
		u64(uint64(h.GetTsMs())),
		field(h.GetNonce()),
		u32(h.GetProtoVersion()),
		field(h.GetEphemeralPubkey()),
		field(Sum256(h.GetCapabilitiesRaw())),
		u64(h.GetConfigVersion()),
	)
}

// HelloMAC 计算 Hello.mac。
func HelloMAC(psk []byte, h *nodev1.Hello) []byte { return hmacSHA256(psk, HelloMACInput(h)) }

// VerifyHelloMAC 以常量时间比较 Hello.mac（NODE-10）。
func VerifyHelloMAC(psk []byte, h *nodev1.Hello) bool {
	if len(psk) == 0 {
		return false
	}
	return hmac.Equal(HelloMAC(psk, h), h.GetMac())
}

// HelloAckMACInput 返回 HelloAck.mac 的输入 M。nodeID 与 agentPub 取自收到的 Hello 原文。
func HelloAckMACInput(nodeID string, agentPub []byte, a *nodev1.HelloAck) []byte {
	return concat(
		field([]byte(helloAckLabel)),
		field([]byte(nodeID)),
		field(agentPub),
		field(a.GetEphemeralPubkey()),
		u32(a.GetProtoVersion()),
		u32(uint32(a.GetSyncMode())),
		field(Sum256(a.GetServerCapabilitiesRaw())),
		u64(a.GetLastReportSeq()),
	)
}

// HelloAckMAC 计算 HelloAck.mac。
func HelloAckMAC(psk []byte, nodeID string, agentPub []byte, a *nodev1.HelloAck) []byte {
	return hmacSHA256(psk, HelloAckMACInput(nodeID, agentPub, a))
}

// VerifyHelloAckMAC 以常量时间比较 HelloAck.mac。
func VerifyHelloAckMAC(psk []byte, nodeID string, agentPub []byte, a *nodev1.HelloAck) bool {
	return hmac.Equal(HelloAckMAC(psk, nodeID, agentPub, a), a.GetMac())
}

// ParseNodeID 把 RFC 9562 规范的小写 UUID 文本转为 16 字节二进制形式。
// 大写、带花括号或 urn 前缀的形式一律拒绝，因为 MAC 覆盖的是文本原文。
func ParseNodeID(text string) ([]byte, error) {
	if len(text) != 36 || text[8] != '-' || text[13] != '-' || text[18] != '-' || text[23] != '-' {
		return nil, fmt.Errorf("nodewire: node_id %q is not a canonical UUID", text)
	}
	if strings.ToLower(text) != text {
		return nil, fmt.Errorf("nodewire: node_id %q is not lowercase", text)
	}
	b, err := hex.DecodeString(strings.ReplaceAll(text, "-", ""))
	if err != nil {
		return nil, fmt.Errorf("nodewire: node_id %q: %w", text, err)
	}
	return b, nil
}

// FormatUUID 把 16 字节格式化为小写 UUID 文本。
func FormatUUID(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// GenerateKeyPair 从 r 读取 32 字节作为 X25519 私钥，返回私钥与公钥。
func GenerateKeyPair(r io.Reader) (priv, pub []byte, err error) {
	priv = make([]byte, KeySize)
	if _, err := io.ReadFull(r, priv); err != nil {
		return nil, nil, fmt.Errorf("nodewire: ephemeral key: %w", err)
	}
	pub, err = curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return nil, nil, fmt.Errorf("nodewire: ephemeral key: %w", err)
	}
	return priv, pub, nil
}

// SessionKeys 按 NODE-11 派生会话密钥：返回 Agent → 控制面与控制面 → Agent 两个方向的密钥。
// ownPriv 为本方临时私钥，peerPub 为对方临时公钥；agentPub、panelPub 按 HKDF info 的顺序给出。
func SessionKeys(ownPriv, peerPub, helloNonce []byte, nodeID string, agentPub, panelPub []byte) (up, down []byte, err error) {
	if len(agentPub) != KeySize || len(panelPub) != KeySize {
		return nil, nil, errors.New("nodewire: ephemeral public key must be 32 bytes")
	}
	shared, err := curve25519.X25519(ownPriv, peerPub)
	if err != nil {
		return nil, nil, fmt.Errorf("nodewire: X25519: %w", err)
	}
	idBin, err := ParseNodeID(nodeID)
	if err != nil {
		return nil, nil, err
	}
	info := concat([]byte(sessionInfo), idBin, agentPub, panelPub)
	keys := make([]byte, 2*KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, helloNonce, info), keys); err != nil {
		return nil, nil, fmt.Errorf("nodewire: HKDF: %w", err)
	}
	return keys[:KeySize], keys[KeySize:], nil
}

func aad(dir byte, seq uint64) []byte { return concat([]byte{dir}, u64(seq)) }

// Sealer 加密一个方向的信封。nonce 每帧从 rand 新读取，不由 seq 派生（NODE-11）。
type Sealer struct {
	aead cipher.AEAD
	dir  byte
	rand io.Reader
}

// NewSealer 返回方向为 dir 的加密器。
func NewSealer(key []byte, dir byte, rand io.Reader) (*Sealer, error) {
	a, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("nodewire: sealer: %w", err)
	}
	return &Sealer{aead: a, dir: dir, rand: rand}, nil
}

// Seal 返回 nonce(24) || 密文与 tag。
func (s *Sealer) Seal(seq uint64, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, SealNonceSize, SealNonceSize+len(plaintext)+chacha20poly1305.Overhead)
	if _, err := io.ReadFull(s.rand, nonce); err != nil {
		return nil, fmt.Errorf("nodewire: nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, aad(s.dir, seq)), nil
}

// Opener 解密一个方向的信封。
type Opener struct {
	aead cipher.AEAD
	dir  byte
}

// NewOpener 返回方向为 dir 的解密器。
func NewOpener(key []byte, dir byte) (*Opener, error) {
	a, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("nodewire: opener: %w", err)
	}
	return &Opener{aead: a, dir: dir}, nil
}

// Open 校验并解密 sealed，返回明文（序列化后的 Envelope）。
func (o *Opener) Open(seq uint64, sealed []byte) ([]byte, error) {
	if len(sealed) < SealOverhead {
		return nil, ErrOpen
	}
	pt, err := o.aead.Open(nil, sealed[:SealNonceSize], sealed[SealNonceSize:], aad(o.dir, seq))
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}

// SnapshotChecksum 返回 SyncFull.checksum：快照原始字节的 SHA-256（NODE-15）。
func SnapshotChecksum(snapshotRaw []byte) []byte { return Sum256(snapshotRaw) }

// VerifySnapshot 以常量时间比较快照校验和。
func VerifySnapshot(sf *nodev1.SyncFull) bool {
	return hmac.Equal(SnapshotChecksum(sf.GetSnapshot()), sf.GetChecksum())
}

// DNSKey 返回 K_dns = HKDF-SHA256(ikm = PSK, salt = 空, info = "akari-dns-secret-v1", L = 32)（NODE-25）。
func DNSKey(psk []byte) ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, psk, nil, []byte(dnsInfo)), k); err != nil {
		return nil, fmt.Errorf("nodewire: dns key: %w", err)
	}
	return k, nil
}

// SealDNSSecret 以 K_dns 加密 DNS 服务商凭据，附加数据为空：nonce(24) || 密文与 tag。
func SealDNSSecret(psk []byte, rand io.Reader, plaintext []byte) ([]byte, error) {
	k, err := DNSKey(psk)
	if err != nil {
		return nil, err
	}
	a, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, SealNonceSize, SealNonceSize+len(plaintext)+chacha20poly1305.Overhead)
	if _, err := io.ReadFull(rand, nonce); err != nil {
		return nil, fmt.Errorf("nodewire: nonce: %w", err)
	}
	return a.Seal(nonce, nonce, plaintext, nil), nil
}

// OpenDNSSecret 解密 DNS 服务商凭据。
func OpenDNSSecret(psk, sealed []byte) ([]byte, error) {
	k, err := DNSKey(psk)
	if err != nil {
		return nil, err
	}
	a, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, err
	}
	if len(sealed) < SealOverhead {
		return nil, ErrOpen
	}
	pt, err := a.Open(nil, sealed[:SealNonceSize], sealed[SealNonceSize:], nil)
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}

// MarshalDeterministic 以确定性顺序序列化 m（能力与快照的原始字节需要可复现）。
func MarshalDeterministic(m proto.Message) ([]byte, error) {
	return proto.MarshalOptions{Deterministic: true}.Marshal(m)
}

// CloseCode 返回 hello_reject 原因对应的 WebSocket 关闭码：4000 + 枚举值（common.proto）。
func CloseCode(r nodev1.HelloRejectReason) int { return 4000 + int(r) }

// ReasonFromCloseCode 把 4001–4007 的关闭码转换为拒绝原因。
func ReasonFromCloseCode(code int) (nodev1.HelloRejectReason, bool) {
	r := nodev1.HelloRejectReason(code - 4000)
	if code > 4000 && r >= nodev1.HelloRejectReason_HELLO_REJECT_REASON_CLOCK_SKEW && r <= nodev1.HelloRejectReason_HELLO_REJECT_REASON_SUPERSEDED {
		return r, true
	}
	return 0, false
}

// AgentUpgradeSignatureInput 返回 AgentUpgrade.signature 的签名输入：
// "akari-agent-upgrade-v1" | version | sha256（messages.proto AgentUpgrade）。
func AgentUpgradeSignatureInput(version string, digest []byte) []byte {
	return concat(field([]byte("akari-agent-upgrade-v1")), field([]byte(version)), field(digest))
}

// VerifyAgentUpgrade 按 key_id 选择公钥验证升级签名（spec/40 DEP-09）。
func VerifyAgentUpgrade(keys map[uint32]ed25519.PublicKey, u *nodev1.AgentUpgrade) bool {
	k, ok := keys[u.GetKeyId()]
	if !ok || len(u.GetSha256()) != sha256.Size {
		return false
	}
	return ed25519.Verify(k, AgentUpgradeSignatureInput(u.GetVersion(), u.GetSha256()), u.GetSignature())
}
