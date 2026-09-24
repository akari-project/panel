// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/akari-project/panel/server/e2e/fakeagent"
	"github.com/akari-project/panel/server/e2e/testgateway"
	"github.com/akari-project/panel/server/internal/clock"
)

// fleet 是同一进程中的一组模拟节点与测试用控制面端。
type fleet struct {
	gw     *testgateway.Server
	ids    []string
	agents []*fakeagent.Agent
}

func scaleNodes(t testing.TB) int {
	if v := os.Getenv("CONFORMANCE_SCALE_NODES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("CONFORMANCE_SCALE_NODES=%q", v)
		}
		return n
	}
	if testing.Short() {
		return 100
	}
	return 1000
}

// newFleet 建档并配置 n 个节点，启动 n 个模拟 Agent（规格默认的时间参数：30 秒上报、NODE-07 退避）。
func newFleet(t testing.TB, n int) *fleet {
	t.Helper()
	f := &fleet{gw: testgateway.Start(t, testgateway.Config{})}
	for i := range n {
		id := f.gw.CreateNode(testgateway.NodeOptions{})
		psk := f.gw.Provision(id)
		f.gw.SetInbounds(id, 0, vlessTCP("vless-tcp"))
		f.gw.UpsertCredentials(id, cred(fmt.Sprintf("cred-%04d", i), fmt.Sprintf("acct-%04d", i)))
		a, err := fakeagent.New(fakeagent.Config{NodeID: id, PSK: psk, StreamURL: f.gw.StreamURL, HTTPClient: f.gw.Client, Seed: uint64(i)})
		if err != nil {
			t.Fatal(err)
		}
		f.ids = append(f.ids, id)
		f.agents = append(f.agents, a)
	}
	t.Cleanup(f.stop)
	return f
}

func (f *fleet) start() {
	for _, a := range f.agents {
		a.Start(context.Background())
	}
}

func (f *fleet) stop() {
	for _, a := range f.agents {
		a.Stop()
	}
}

// waitConverged 等待全部节点在当前会话中上报了控制面当前的版本，且会话代次不小于 minGen。
func (f *fleet) waitConverged(t testing.TB, minGen int, timeout time.Duration) time.Duration {
	t.Helper()
	real := clock.Real{}
	start := real.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	want := len(f.ids)
	last := 0
	ok := f.gw.Wait(ctx, func(v testgateway.View) bool {
		last = v.Count(func(s testgateway.Summary) bool {
			return s.Connected && s.SessionGen >= minGen && s.StatusGen == s.SessionGen && s.AppliedVersion == s.Version
		})
		return last == want
	})
	if !ok {
		t.Fatalf("only %d of %d nodes converged within %v", last, want, timeout)
	}
	return real.Now().Sub(start)
}

type scaleResult struct {
	Nodes              int     `json:"nodes"`
	Race               bool    `json:"race"`
	GOMAXPROCS         int     `json:"gomaxprocs"`
	InitialConvergeSec float64 `json:"initial_converge_seconds"`
	ReconnectSec       float64 `json:"reconnect_converge_seconds"`
	PushConvergeSec    float64 `json:"push_converge_seconds"`
	Goroutines         int     `json:"goroutines"`
	GoroutinesPerNode  float64 `json:"goroutines_per_node"`
	HeapInuseMiB       float64 `json:"heap_inuse_mib"`
	HeapPerNodeKiB     float64 `json:"heap_per_node_kib"`
	SysMiB             float64 `json:"sys_mib"`
}

func memSnapshot() (runtime.MemStats, int) {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m, runtime.NumGoroutine()
}

// TestScale_1000Nodes：同一进程同时模拟 1,000 个节点（M0-07 验收 2、spec/20 20.6 规模）：
// 全部接入并收敛；控制面同时断开全部连接后全部重连并收敛；再向全部节点下发一次凭据变更并收敛。
// 内存与协程数包含测试用控制面端一侧（每个节点一条会话）。
func TestScale_1000Nodes(t *testing.T) {
	n := scaleNodes(t)
	base, baseG := memSnapshot()
	f := newFleet(t, n)
	f.start()
	res := scaleResult{Nodes: n, Race: raceEnabled, GOMAXPROCS: runtime.GOMAXPROCS(0)}
	res.InitialConvergeSec = f.waitConverged(t, 1, 3*time.Minute).Seconds()

	m, g := memSnapshot()
	res.Goroutines = g - baseG
	res.GoroutinesPerNode = float64(res.Goroutines) / float64(n)
	heap := float64(int64(m.HeapInuse) - int64(base.HeapInuse)) // GC 后可能小于基线，按有符号数相减
	res.HeapInuseMiB = heap / (1 << 20)
	res.HeapPerNodeKiB = heap / 1024 / float64(n)
	res.SysMiB = float64(m.Sys) / (1 << 20)

	// 1,000 个节点同时断线重连（20.6）。收敛：全部 online 且已应用的版本等于控制面当前值（spec/42 42.4）。
	if dropped := f.gw.DropAll(); dropped != n {
		t.Fatalf("dropped %d sessions, want %d", dropped, n)
	}
	res.ReconnectSec = f.waitConverged(t, 2, 3*time.Minute).Seconds()

	// 向全部在线节点各下发一条凭据变更，计时从第一条下发开始。
	real := clock.Real{}
	pushStart := real.Now()
	for i, id := range f.ids {
		f.gw.UpsertCredentials(id, cred(fmt.Sprintf("cred-new-%04d", i), fmt.Sprintf("acct-%04d", i)))
	}
	f.waitConverged(t, 2, time.Minute)
	res.PushConvergeSec = real.Now().Sub(pushStart).Seconds()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	online := 0
	f.gw.Wait(ctx, func(v testgateway.View) bool {
		online = v.Count(func(s testgateway.Summary) bool { return s.State == testgateway.StateOnline })
		return online == n
	})
	if online != n {
		t.Fatalf("%d of %d nodes online", online, n)
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	t.Logf("scale result:\n%s", b)
	if out := os.Getenv("CONFORMANCE_SCALE_OUT"); out != "" {
		if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// BenchmarkReconnect1000：每次迭代断开全部节点并等待全部重连收敛。
//
//	go test ./e2e/conformance -run '^$' -bench Reconnect1000 -benchtime 3x
func BenchmarkReconnect1000(b *testing.B) {
	n := scaleNodes(b)
	f := newFleet(b, n)
	f.start()
	f.waitConverged(b, 1, 3*time.Minute)
	m, g := memSnapshot()
	b.ResetTimer()
	for i := range b.N {
		f.gw.DropAll()
		f.waitConverged(b, i+2, 3*time.Minute)
	}
	b.StopTimer()
	b.ReportMetric(float64(g)/float64(n), "goroutines/node")
	b.ReportMetric(float64(m.HeapInuse)/1024/float64(n), "heap-KiB/node")
}
