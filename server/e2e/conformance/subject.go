// SPDX-License-Identifier: AGPL-3.0-or-later

// Package conformance 是节点协议一致性套件（spec/20 20.6、spec/42 42.3）。
//
// 被测对象是 Agent（Subject）：默认是模拟 Agent（e2e/fakeagent），设置 CONFORMANCE_AGENT=exec 时
// 改为外部进程（真实 node-agent，M3），用法见 README.md。控制面一侧目前由 e2e/testgateway 充当；
// 以 TestGateway_ 开头的用例是网关断言，M2-02 之后改为对 internal/gateway 运行。
//
// 故障一律在控制面一侧注入（断开连接、丢帧、重复帧、暂停读取、注入 hello_reject），
// 因此同一套用例对任何 Agent 实现都成立，不依赖被测对象内部的钩子。
// 需要产生流量或模拟代理连接的用例通过可选接口（TrafficSource、ConnectionSource）驱动被测对象，
// 被测对象不支持时跳过；StateInspector 提供额外的白盒断言。
package conformance

import (
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"
)

// Subject 是被测的 Agent 实现。
type Subject interface {
	Name() string
	Timing() Timing
	// Start 启动一个新装的 Agent：以接入令牌向 GatewayURL 接入（NODE-02），然后连接并保持在线。
	Start(t testing.TB, opts StartOptions) Instance
}

// StartOptions 是启动 Agent 的参数。
type StartOptions struct {
	GatewayURL  string       // https://<gateway>
	EnrollToken string       // 一次性接入令牌
	Client      *http.Client // 信任测试证书的 HTTP 客户端（进程内实现使用）
	CAFile      string       // 测试证书的 PEM 文件（外部进程使用）
	RootCAs     *x509.CertPool
	// Capabilities 是进程内实现上报的能力；外部进程忽略（能力来自二进制本身，AGT-08）。
	Capabilities *nodev1.Capabilities
}

// Instance 是一个运行中的 Agent。
type Instance interface {
	// Stop 优雅停止（SIGTERM），保留本地状态。
	Stop()
	// Restart 以保留的本地状态重新启动（AGT-05），不重新接入。
	Restart(t testing.TB)
	// Clone 以同一身份（复制本地状态）再启动一个实例，模拟同一节点重复运行（NODE-21）。
	Clone(t testing.TB) Instance
}

// TrafficSource 由能按凭据产生已知字节数流量的实现提供（spec/21 AGT-12 的计量口径）。
type TrafficSource interface {
	AddTraffic(credentialID string, up, down uint64)
	// FlushTraffic 立即生成一份流量报告（真实 Agent 可以等待下一个上报周期）。
	FlushTraffic()
}

// ConnectionSource 由能模拟账号代理连接的实现提供（租约，spec/22 ACC-08–10）。
type ConnectionSource interface {
	OpenConnection(account string) bool
	CloseConnection(account string)
}

// AppliedState 是 Agent 已应用的配置。
type AppliedState struct {
	ConfigVersion uint64
	Credentials   []string // 已排序
	Kernel        nodev1.KernelType
	// Versions 是按应用顺序记录的配置版本；每个版本至多出现一次（“不重”）。
	Versions []uint64
}

// StateInspector 由能报告内部已应用状态的实现提供。
type StateInspector interface {
	Applied() AppliedState
}

// Timing 描述被测实现的时间参数，用于确定等待上限。
type Timing struct {
	// BackoffScale 是实现对 NODE-07、NODE-22 重试等待的缩放比例；真实 Agent 为 1。
	BackoffScale float64
	// StatusInterval 是 ReportStatus 的周期（NODE-20 为 30 秒）。
	StatusInterval time.Duration
	// Patience 是一般状态收敛的等待上限。
	Patience time.Duration
	// WindowMessages 是节点方向的在途窗口（NODE-14，默认 1,000）。
	WindowMessages int
}

// Hour 返回按缩放比例换算的 1 小时，即 hourly 类拒绝原因的重试间隔。
func (t Timing) Hour() time.Duration { return time.Duration(float64(time.Hour) * t.BackoffScale) }

// FirstRetryBound 返回首次断线重连等待的上限 min(30s, 1s × 2⁰) 加上握手余量。
func (t Timing) FirstRetryBound() time.Duration {
	return time.Duration(float64(2*time.Second)*t.BackoffScale) + 5*time.Second
}

// SelectSubject 按环境变量 CONFORMANCE_AGENT 选择被测实现：fake（默认）或 exec。
func SelectSubject(t testing.TB) Subject {
	t.Helper()
	switch v := os.Getenv("CONFORMANCE_AGENT"); v {
	case "", "fake":
		return FakeSubject{}
	case "exec":
		s, err := ExecSubjectFromEnv()
		if err != nil {
			t.Fatalf("conformance: %v", err)
		}
		return s
	default:
		t.Fatalf("conformance: unknown CONFORMANCE_AGENT=%q (fake, exec)", v)
		return nil
	}
}

// errf 是给外部实现的错误格式。
func errf(format string, args ...any) error { return fmt.Errorf("conformance: "+format, args...) }
