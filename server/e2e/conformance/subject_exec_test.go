// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"errors"
	"syscall"
	"testing"
)

// TestExecStopWaitsForProcessGroup：Stop 返回时进程组内的进程都已退出。
// 回归：曾只等待 sh 退出，Agent 仍在退出时 Restart 会让两个进程共用状态目录。
func TestExecStopWaitsForProcessGroup(t *testing.T) {
	t.Parallel()
	// 外层 sh 收到 SIGTERM 立即退出；内层进程处理 SIGTERM 需要约 1 秒。
	s := &ExecSubject{Command: `sh -c 'trap "sleep 1; exit 0" TERM; while :; do sleep 0.05; done' & wait`}
	p := &execInstance{s: s, stateDir: t.TempDir()}
	p.start(t, s.Command, "")
	pgid := p.cmd.Process.Pid
	p.Stop()
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process group %d still alive after Stop: %v", pgid, err)
	}
	if p.log != nil {
		t.Fatal("log file not released")
	}
}
