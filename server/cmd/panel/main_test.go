// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akari-project/panel/server/internal/httpx"
	"github.com/akari-project/panel/server/internal/testdb"
	"github.com/akari-project/panel/server/internal/testkv"
)

// buildBinary 以 noui 构建 panel，测试真实进程的子命令与信号处理。
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "panel")
	cmd := exec.Command("go", "build", "-tags", "noui", "-o", bin, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}
	return bin
}

// valkeyURL 由 TestBinary 设置：api 角色需要 Valkey（spec/10 AUTH-06）。
var valkeyURL string

func command(bin, dbURL string, args ...string) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(),
		"PANEL_CONFIG=",
		"PANEL_DATABASE_URL="+dbURL,
		"PANEL_HTTP_LISTEN=127.0.0.1:0",
		"PANEL_GATEWAY_LISTEN=127.0.0.1:0",
		"PANEL_WORKER_LISTEN=127.0.0.1:0",
		"PANEL_HTTP_SHUTDOWN_TIMEOUT=5s",
		"PANEL_MASTER_KEY=1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
		"PANEL_TOKEN_KEY=1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)),
		"PANEL_VALKEY_URL="+valkeyURL,
	)
	return cmd
}

// M0-05 验收 2 与 3：`panel migrate` 在空库上执行全部迁移；各角色作为独立进程启动、响应 /healthz、收到 SIGTERM 后正常退出。
func TestBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	pool, dbURL := testdb.NewWithURL(t)
	_, valkeyURL = testkv.NewWithURL(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	bin := buildBinary(t)

	// 迁移前启动角色必须失败（DEP-12：不自动迁移）。
	out, err := command(bin, dbURL, "api").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "panel migrate") {
		t.Fatalf("api before migrate: err=%v out=%s", err, out)
	}

	out, err = command(bin, dbURL, "migrate").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "database at version") {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	out, err = command(bin, dbURL, "migrate", "status").Output()
	if err != nil || !strings.Contains(string(out), "00001  applied") {
		t.Fatalf("migrate status: %v\n%s", err, out)
	}

	for _, mode := range []string{"api", "gateway", "worker", "all"} {
		t.Run(mode, func(t *testing.T) { runRole(t, bin, dbURL, mode) })
	}

	t.Run("admin create", func(t *testing.T) {
		cmd := command(bin, dbURL, "admin", "create", "--email", "root@example.com", "--password-stdin")
		cmd.Stdin = strings.NewReader("correct horse battery\n")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "superadmin created") {
			t.Fatalf("admin create: %v\n%s", err, out)
		}
		if strings.Contains(string(out), "correct horse battery") {
			t.Error("password echoed in output")
		}
		cmd = command(bin, dbURL, "admin", "create", "--email", "other@example.com", "--password-stdin")
		cmd.Stdin = strings.NewReader("correct horse battery\n")
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("second admin create succeeded: %s", out)
		}
	})
}

type logLine struct {
	Msg      string `json:"msg"`
	Addr     string `json:"addr"`
	Listener string `json:"listener"`
}

func runRole(t *testing.T, bin, dbURL, mode string) {
	cmd := command(bin, dbURL, mode)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	addrc := make(chan string, 1)
	var logs bytes.Buffer
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			logs.Write(sc.Bytes())
			logs.WriteByte('\n')
			var l logLine
			if json.Unmarshal(sc.Bytes(), &l) == nil && l.Msg == "listening" {
				select {
				case addrc <- l.Addr:
				default:
				}
			}
		}
	}()
	var addr string
	select {
	case addr = <-addrc:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s did not start listening", mode)
	}

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var h httpx.Health
	if err := json.Unmarshal(body, &h); err != nil || resp.StatusCode != 200 || h.Database != "ok" {
		t.Fatalf("/healthz = %d %s", resp.StatusCode, body)
	}
	wantRoles := mode
	if mode == "all" {
		wantRoles = "api,gateway,worker"
	}
	if strings.Join(h.Roles, ",") != wantRoles {
		t.Errorf("roles = %v, want %s", h.Roles, wantRoles)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// 标准错误读完（进程退出）之后才能调用 Wait。
	select {
	case <-scanDone:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s did not exit after SIGTERM", mode)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("%s exit after SIGTERM: %v\n%s", mode, err, logs.String())
	}
	if !strings.Contains(logs.String(), `"msg":"stopped"`) {
		t.Errorf("%s did not log a graceful stop:\n%s", mode, logs.String())
	}
}

func TestUsageErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), nil, nil, &out, &errOut); err == nil {
		t.Error("no command accepted")
	}
	if err := run(context.Background(), []string{"server"}, nil, &out, &errOut); err == nil {
		t.Error("unknown command accepted")
	}
	if err := run(context.Background(), []string{"version"}, nil, &out, &errOut); err != nil || !strings.Contains(out.String(), "panel ") {
		t.Errorf("version: %v %q", err, out.String())
	}
}
