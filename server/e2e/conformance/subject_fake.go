// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/fakeagent"
	"github.com/akari-project/panel/server/e2e/nodewire"
)

// FakeSubject 以进程内的模拟 Agent 作为被测对象。时间参数按比例缩短以加快套件。
type FakeSubject struct{}

// fakeTiming 是模拟 Agent 在套件中使用的时间参数：退避缩放为 2%（hourly 类原因约 72 秒），
// 窗口缩小到 64 条以便覆盖背压。
var fakeTiming = fakeagent.Timing{
	StatusInterval:  200 * time.Millisecond,
	TrafficInterval: 100 * time.Millisecond,
	BackoffScale:    0.02,
	WindowMessages:  64,
}

// Name 实现 Subject。
func (FakeSubject) Name() string { return "fakeagent" }

// Timing 实现 Subject。
func (FakeSubject) Timing() Timing {
	return Timing{BackoffScale: fakeTiming.BackoffScale, StatusInterval: fakeTiming.StatusInterval, Patience: 20 * time.Second, WindowMessages: fakeTiming.WindowMessages}
}

// Start 实现 Subject：接入后启动模拟 Agent。
func (FakeSubject) Start(t testing.TB, opts StartOptions) Instance {
	t.Helper()
	caps := opts.Capabilities
	if caps == nil {
		caps = fakeagent.DefaultCapabilities()
	}
	capsRaw, err := nodewire.MarshalDeterministic(caps)
	if err != nil {
		t.Fatal(err)
	}
	fp := make([]byte, 32)
	_, _ = rand.Read(fp)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := fakeagent.Enroll(ctx, opts.Client, opts.GatewayURL, &nodev1.EnrollRequest{
		EnrollToken:     opts.EnrollToken,
		Host:            &nodev1.HostInfo{Hostname: "fake", Os: "linux", Arch: "amd64", AgentVersion: fakeagent.Version},
		CapabilitiesRaw: capsRaw,
		HostFingerprint: hex.EncodeToString(fp),
	})
	if err != nil {
		t.Fatalf("fakeagent: enroll: %v", err)
	}
	inst := &FakeInstance{cfg: fakeagent.Config{
		NodeID:       resp.GetNodeId(),
		PSK:          resp.GetPsk(),
		StreamURL:    resp.GetStreamUrl(),
		HTTPClient:   opts.Client,
		Capabilities: caps,
		Timing:       fakeTiming,
	}}
	inst.start(t)
	t.Cleanup(inst.Stop)
	return inst
}

// FakeInstance 是运行中的模拟 Agent。
type FakeInstance struct {
	cfg fakeagent.Config

	mu sync.Mutex
	a  *fakeagent.Agent
}

func (f *FakeInstance) start(t testing.TB) {
	f.mu.Lock()
	cfg := f.cfg
	f.mu.Unlock()
	a, err := fakeagent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Start(context.Background())
	f.mu.Lock()
	f.a = a
	f.mu.Unlock()
}

// Agent 返回当前的模拟 Agent。
func (f *FakeInstance) Agent() *fakeagent.Agent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.a
}

// Stop 实现 Instance。
func (f *FakeInstance) Stop() { f.Agent().Stop() }

// Restart 实现 Instance。
func (f *FakeInstance) Restart(t testing.TB) {
	a := f.Agent()
	a.Stop()
	st := a.PersistedState()
	f.mu.Lock()
	f.cfg.State = st
	f.mu.Unlock()
	f.start(t)
}

// Clone 实现 Instance。
func (f *FakeInstance) Clone(t testing.TB) Instance {
	st := f.Agent().PersistedState()
	st.Dedup = nil // 另一个进程：复制状态文件，去重记录各自维护
	f.mu.Lock()
	c := &FakeInstance{cfg: f.cfg}
	f.mu.Unlock()
	c.cfg.State = st
	c.cfg.Seed++
	c.start(t)
	t.Cleanup(c.Stop)
	return c
}

// AddTraffic 实现 TrafficSource。
func (f *FakeInstance) AddTraffic(credentialID string, up, down uint64) {
	f.Agent().AddTraffic(credentialID, up, down)
}

// FlushTraffic 实现 TrafficSource。
func (f *FakeInstance) FlushTraffic() { f.Agent().FlushTraffic() }

// OpenConnection 实现 ConnectionSource。
func (f *FakeInstance) OpenConnection(account string) bool { return f.Agent().OpenConnection(account) }

// CloseConnection 实现 ConnectionSource。
func (f *FakeInstance) CloseConnection(account string) { f.Agent().CloseConnection(account) }

// Applied 实现 StateInspector。
func (f *FakeInstance) Applied() AppliedState {
	v := f.Agent().View()
	s := AppliedState{ConfigVersion: v.ConfigVersion, Credentials: v.Credentials, Kernel: v.Kernel}
	for _, a := range v.Applied {
		s.Versions = append(s.Versions, a.Version)
	}
	return s
}
