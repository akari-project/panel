// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ExecSubject 以外部进程（真实 node-agent）作为被测对象。
//
// 命令模板中的占位符：
//   - {server}：控制面地址 https://<gateway>（spec/21 21.5 的 --server）；
//   - {token}：接入令牌（--enroll-token）；重启与克隆时为空串；
//   - {state_dir}：本地状态目录（AGT-05），每个实例独立；
//   - {ca_file}：测试证书的 PEM 文件，同时以 SSL_CERT_FILE 环境变量传入。
//
// 进程以 sh -c 执行，停止时发送 SIGTERM，最多等待 35 秒（spec/21 21.4 第 4 步）后强制结束。
type ExecSubject struct {
	Command        string // 首次启动：接入并连接
	RestartCommand string // 以已有状态目录启动；为空时使用 Command
	timing         Timing
}

// ExecSubjectFromEnv 从环境变量构造 ExecSubject：
//   - CONFORMANCE_AGENT_CMD（必填）、CONFORMANCE_AGENT_RESTART_CMD；
//   - CONFORMANCE_BACKOFF_SCALE（默认 1）、CONFORMANCE_STATUS_INTERVAL（默认 30s）、
//     CONFORMANCE_PATIENCE（默认 120s）、CONFORMANCE_WINDOW（默认 1000）。
func ExecSubjectFromEnv() (*ExecSubject, error) {
	cmd := os.Getenv("CONFORMANCE_AGENT_CMD")
	if cmd == "" {
		return nil, errors.New("CONFORMANCE_AGENT=exec requires CONFORMANCE_AGENT_CMD")
	}
	s := &ExecSubject{
		Command:        cmd,
		RestartCommand: os.Getenv("CONFORMANCE_AGENT_RESTART_CMD"),
		timing:         Timing{BackoffScale: 1, StatusInterval: 30 * time.Second, Patience: 120 * time.Second, WindowMessages: 1000},
	}
	if v := os.Getenv("CONFORMANCE_BACKOFF_SCALE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			return nil, errf("CONFORMANCE_BACKOFF_SCALE=%q", v)
		}
		s.timing.BackoffScale = f
	}
	for env, dst := range map[string]*time.Duration{"CONFORMANCE_STATUS_INTERVAL": &s.timing.StatusInterval, "CONFORMANCE_PATIENCE": &s.timing.Patience} {
		if v := os.Getenv(env); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, errf("%s=%q: %v", env, v, err)
			}
			*dst = d
		}
	}
	if v := os.Getenv("CONFORMANCE_WINDOW"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, errf("CONFORMANCE_WINDOW=%q", v)
		}
		s.timing.WindowMessages = n
	}
	return s, nil
}

// Name 实现 Subject。
func (s *ExecSubject) Name() string { return "exec" }

// Timing 实现 Subject。
func (s *ExecSubject) Timing() Timing { return s.timing }

// Start 实现 Subject。
func (s *ExecSubject) Start(t testing.TB, opts StartOptions) Instance {
	t.Helper()
	p := &execInstance{s: s, server: opts.GatewayURL, caFile: opts.CAFile, stateDir: t.TempDir()}
	p.start(t, s.Command, opts.EnrollToken)
	t.Cleanup(p.Stop)
	return p
}

type execInstance struct {
	s        *ExecSubject
	server   string
	caFile   string
	stateDir string

	mu  sync.Mutex
	cmd *exec.Cmd
	log *os.File
}

func (p *execInstance) expand(tmpl, token string) string {
	return strings.NewReplacer("{server}", p.server, "{token}", token, "{state_dir}", p.stateDir, "{ca_file}", p.caFile).Replace(tmpl)
}

func (p *execInstance) start(t testing.TB, tmpl, token string) {
	t.Helper()
	logFile, err := os.CreateTemp(t.TempDir(), "agent-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", p.expand(tmpl, token))
	cmd.Env = append(os.Environ(), "SSL_CERT_FILE="+p.caFile)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("conformance: start agent: %v", err)
	}
	p.mu.Lock()
	p.cmd, p.log = cmd, logFile
	p.mu.Unlock()
	t.Cleanup(func() {
		if t.Failed() {
			if b, err := os.ReadFile(logFile.Name()); err == nil {
				t.Logf("agent output (%s):\n%s", logFile.Name(), tail(b, 8<<10))
			}
		}
	})
}

func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

// Stop 向进程组发送 SIGTERM，等待组内全部进程退出，最多 35 秒，超时后对进程组 SIGKILL。
// 只等 sh 退出不够：Agent 可能仍在退出，此时 Restart 会让两个进程共用状态目录。
func (p *execInstance) Stop() {
	p.mu.Lock()
	cmd, logFile := p.cmd, p.log
	p.cmd, p.log = nil, nil
	p.mu.Unlock()
	if logFile != nil {
		defer logFile.Close()
	}
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	deadline := time.NewTimer(35 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	shExited := false
	for {
		select {
		case <-done:
			shExited, done = true, nil
		case <-poll.C:
		case <-deadline.C:
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			if !shExited {
				<-done
			}
			waitGroupGone(pgid)
			return
		}
		if shExited && errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
			return
		}
	}
}

// waitGroupGone 在 SIGKILL 之后等待进程组消失（最多 5 秒）。
func waitGroupGone(pgid int) {
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	limit := time.NewTimer(5 * time.Second)
	defer limit.Stop()
	for !errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
		select {
		case <-poll.C:
		case <-limit.C:
			return
		}
	}
}

func (p *execInstance) restartTemplate() string {
	if p.s.RestartCommand != "" {
		return p.s.RestartCommand
	}
	return p.s.Command
}

// Restart 实现 Instance。
func (p *execInstance) Restart(t testing.TB) {
	p.Stop()
	p.start(t, p.restartTemplate(), "")
}

// Clone 复制状态目录后以同一身份启动第二个进程。
func (p *execInstance) Clone(t testing.TB) Instance {
	c := &execInstance{s: p.s, server: p.server, caFile: p.caFile, stateDir: t.TempDir()}
	if err := copyDir(p.stateDir, c.stateDir); err != nil {
		t.Fatalf("conformance: copy state dir: %v", err)
	}
	c.start(t, p.restartTemplate(), "")
	t.Cleanup(c.Stop)
	return c
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
}
