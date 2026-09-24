// SPDX-License-Identifier: AGPL-3.0-or-later

package fakeagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/akari-project/panel/server/e2e/testgateway"
)

// TestStopSendsGoingAway：Stop 以关闭码 1001 关闭会话，而不是直接断开连接。
// 回归：会话 ctx 曾挂在 Agent 的 ctx 上，取消时 coder/websocket 直接关闭连接，控制面收不到 1001。
func TestStopSendsGoingAway(t *testing.T) {
	gw := testgateway.Start(t, testgateway.Config{})
	for i := range 5 {
		id := gw.CreateNode(testgateway.NodeOptions{})
		psk := gw.Provision(id)
		a, err := New(Config{NodeID: id, PSK: psk, StreamURL: gw.StreamURL, HTTPClient: gw.Client})
		if err != nil {
			t.Fatal(err)
		}
		a.Start(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		// 以 Agent 一侧的握手完成为准：控制面在写出 hello_ack 之前就登记了会话。
		if _, ok := a.WaitEvent(ctx, func(e Event) bool { return e.Kind == EventConnected }); !ok {
			t.Fatal("not connected")
		}
		a.Stop()
		var last string
		gw.WaitNode(ctx, id, func(n testgateway.NodeInfo) bool {
			if len(n.Closes) > 0 {
				last = n.Closes[0].Err
				return true
			}
			return false
		})
		cancel()
		if !strings.Contains(last, "1001") && !strings.Contains(last, "GoingAway") {
			t.Fatalf("stop %d: control plane saw %q, want close code 1001", i, last)
		}
	}
}
