// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fakeagent 是模拟 Agent：完整实现节点协议（spec/20），但不运行内核。
//
// 实现的内容：接入（NODE-02）、握手与会话加密（NODE-08–11）、信封、seq/ack 与重连重传
// （NODE-12–14）、带版本指令的应用规则（NODE-23）、状态与流量上报（NODE-20、spec/22 ACC-03）、
// 配额租约请求（ACC-08–10）、hello_reject 各原因的重试策略（NODE-22）。
//
// 测试可以编程注入断线、延迟、重复与丢包（nodewire.FaultPlan），可以配置上报的内核能力
// （Config.Capabilities 的 kernels，spec/21 AGT-08），并通过 View 与 Events 观察内部状态。
// 模拟 Agent 用于 M2 联调控制面与 e2e/conformance 一致性套件；真实 Agent 必须通过同一套件。
package fakeagent

import (
	"crypto/ed25519"
	"io"
	"log/slog"
	"net/http"
	"time"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/nodewire"
	"github.com/akari-project/panel/server/internal/clock"
)

// Version 是模拟 Agent 上报的 agent_version。
const Version = "0.0.0-fakeagent"

// Config 配置一个模拟 Agent。
type Config struct {
	NodeID     string
	PSK        []byte // 节点密钥；只在内存中使用，不写日志（CONV-24）
	StreamURL  string // wss://gateway.<运营者域名>/v1/stream
	HTTPClient *http.Client

	Clock  clock.Clock // 默认 clock.Real
	Rand   io.Reader   // 临时密钥、nonce、idem_key；默认 crypto/rand
	Seed   uint64      // 退避抖动的随机种子
	Logger *slog.Logger

	// Capabilities 是握手时上报的能力；nil 时取 DefaultCapabilities()。
	// 其中 kernels 决定模拟 Agent 能切换到哪些内核、接受哪些协议与传输（AGT-07、AGT-08）。
	Capabilities *nodev1.Capabilities
	ProtoVersion uint32 // 默认 nodewire.ProtoVersion

	Faults nodewire.FaultPlan // 会话帧故障注入；运行中替换可以用 *nodewire.FaultSwitch
	Timing Timing

	// State 是上一次运行保存的本地状态（AGT-05），用于模拟重启；nil 表示新装。
	State *State

	// InboundFailures 让指定 tag 的入站上报 listening=false 与给定错误。
	InboundFailures map[string]string
	// UpgradeKeys 是验证 AgentUpgrade 签名的公钥（DEP-09）；为空时只记录不验证。
	UpgradeKeys map[uint32]ed25519.PublicKey
}

// Timing 是模拟 Agent 的时间参数，零值字段取规格默认值。
type Timing struct {
	StatusInterval   time.Duration // ReportStatus 周期，默认 30 秒（NODE-20）
	TrafficInterval  time.Duration // ReportTraffic 周期，默认 30 秒（spec/21 21.4）
	PingInterval     time.Duration // 默认 25 秒（NODE-07）
	PingMisses       int           // 连续无响应次数，默认 3
	HandshakeTimeout time.Duration // 默认 5 秒（20.3 第 5 步）
	LeaseGrace       time.Duration // 首个连接等待租约的时间，默认 5 秒（ACC-08）
	// BackoffScale 按比例缩放重试等待（NODE-07、NODE-22；busy 的 retry_after_ms 除外），默认 1；测试用较小的值加速。
	BackoffScale float64

	WindowMessages int // 在途窗口，默认 1,000 条（NODE-14）
	WindowBytes    int // 默认 8 MiB
}

func (t Timing) withDefaults() Timing {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&t.StatusInterval, 30*time.Second)
	def(&t.TrafficInterval, 30*time.Second)
	def(&t.PingInterval, 25*time.Second)
	def(&t.HandshakeTimeout, 5*time.Second)
	def(&t.LeaseGrace, 5*time.Second)
	if t.PingMisses <= 0 {
		t.PingMisses = 3
	}
	if t.BackoffScale <= 0 {
		t.BackoffScale = 1
	}
	if t.WindowMessages <= 0 {
		t.WindowMessages = nodewire.DefaultWindowMessages
	}
	if t.WindowBytes <= 0 {
		t.WindowBytes = nodewire.DefaultWindowBytes
	}
	return t
}

// 两个内核支持的协议与传输，取自 spec/21 21.2 的矩阵，只用于模拟上报。
var (
	singboxProtocols = []nodev1.Protocol{
		nodev1.Protocol_PROTOCOL_VLESS, nodev1.Protocol_PROTOCOL_VMESS, nodev1.Protocol_PROTOCOL_TROJAN,
		nodev1.Protocol_PROTOCOL_SHADOWSOCKS, nodev1.Protocol_PROTOCOL_HYSTERIA2, nodev1.Protocol_PROTOCOL_TUIC,
		nodev1.Protocol_PROTOCOL_ANYTLS,
	}
	singboxTransports = []nodev1.Transport{
		nodev1.Transport_TRANSPORT_TCP, nodev1.Transport_TRANSPORT_WS, nodev1.Transport_TRANSPORT_GRPC,
		nodev1.Transport_TRANSPORT_HTTPUPGRADE, nodev1.Transport_TRANSPORT_QUIC,
	}
	xrayProtocols = []nodev1.Protocol{
		nodev1.Protocol_PROTOCOL_VLESS, nodev1.Protocol_PROTOCOL_VMESS, nodev1.Protocol_PROTOCOL_TROJAN,
		nodev1.Protocol_PROTOCOL_SHADOWSOCKS,
	}
	xrayTransports = []nodev1.Transport{
		nodev1.Transport_TRANSPORT_TCP, nodev1.Transport_TRANSPORT_WS, nodev1.Transport_TRANSPORT_GRPC,
		nodev1.Transport_TRANSPORT_HTTPUPGRADE, nodev1.Transport_TRANSPORT_XHTTP, nodev1.Transport_TRANSPORT_MKCP,
	}
)

// SingboxSupport 返回模拟的 sing-box 内核能力。
func SingboxSupport() *nodev1.KernelSupport {
	return &nodev1.KernelSupport{
		Kernel: nodev1.KernelType_KERNEL_TYPE_SINGBOX, Version: "1.12.0-fake",
		StableProtocols: singboxProtocols, StableTransports: singboxTransports,
	}
}

// XraySupport 返回模拟的 Xray-core 内核能力。
func XraySupport() *nodev1.KernelSupport {
	return &nodev1.KernelSupport{
		Kernel: nodev1.KernelType_KERNEL_TYPE_XRAY, Version: "26.1.0-fake",
		StableProtocols: xrayProtocols, StableTransports: xrayTransports,
		ExperimentalProtocols:  []nodev1.Protocol{nodev1.Protocol_PROTOCOL_HYSTERIA2},
		ExperimentalTransports: []nodev1.Transport{nodev1.Transport_TRANSPORT_QUIC},
	}
}

// DefaultCapabilities 返回内置两个内核、当前运行 sing-box 的能力（AGT-08）。
func DefaultCapabilities() *nodev1.Capabilities {
	return CapabilitiesWith(SingboxSupport(), XraySupport())
}

// CapabilitiesWith 返回内置给定内核的能力，当前运行第一个内核。
func CapabilitiesWith(kernels ...*nodev1.KernelSupport) *nodev1.Capabilities {
	c := &nodev1.Capabilities{
		AgentVersion:       Version,
		DeployMode:         nodev1.DeployMode_DEPLOY_MODE_NODE,
		SupportsQuotaLease: true,
		Kernels:            kernels,
	}
	if len(kernels) > 0 {
		c.Kernel, c.KernelVersion = kernels[0].GetKernel(), kernels[0].GetVersion()
		c.Protocols = kernels[0].GetStableProtocols()
	}
	return c
}
