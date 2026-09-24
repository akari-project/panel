// SPDX-License-Identifier: AGPL-3.0-or-later

// Package portal 在真实控制面上运行用户中心的 Playwright 测试（backlog M1-01 验收 4）。
//
// 本测试启动 PostgreSQL、Valkey、Mailpit（testcontainers），把 web 的构建产物按生产方式准备后，
// 以 all 模式在进程内启动控制面（真实时钟：浏览器中的 TOTP 按真实时间计算），再运行
// web/playwright.real.config.ts，环境变量 PORTAL_URL 与 MAILPIT_URL 指向本次启动的服务。
//
// 需要先构建前端（pnpm -r build），并设置 PANEL_E2E_PLAYWRIGHT=1；由 make e2e-portal 运行。
package portal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/akari-project/panel/server/internal/app"
	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/clientconfig"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/config"
	"github.com/akari-project/panel/server/internal/secretbox"
	"github.com/akari-project/panel/server/internal/testdb"
	"github.com/akari-project/panel/server/internal/testkv"
)

// MailpitImage 与 compose.dev.yaml 一致。
const MailpitImage = "axllent/mailpit"

// syncBuffer 收集控制面日志，失败时输出。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) tail(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.b.Bytes()
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

func startMailpit(t *testing.T) (smtpHost string, smtpPort int, apiURL string) {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        MailpitImage,
			ExposedPorts: []string{"1025/tcp", "8025/tcp"},
			WaitingFor:   wait.ForHTTP("/readyz").WithPort("8025/tcp").WithStartupTimeout(time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("mailpit: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	smtp, err := c.MappedPort(ctx, "1025/tcp")
	if err != nil {
		t.Fatal(err)
	}
	api, err := c.PortEndpoint(ctx, "8025/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	return host, int(smtp.Num()), api
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func keyEnv(b byte) string {
	return "1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

// TestM1_01_PortalPlaywright：注册、邮箱验证、登录、启用 TOTP 与二次验证登录、找回密码（M1-01 验收 4）。
func TestM1_01_PortalPlaywright(t *testing.T) {
	if os.Getenv("PANEL_E2E_PLAYWRIGHT") != "1" {
		t.Skip("set PANEL_E2E_PLAYWRIGHT=1 (make e2e-portal) to run the portal Playwright suite against a real control plane")
	}
	web, err := filepath.Abs("../../../web")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, dbURL := testdb.NewWithURL(t)
	kvc, _ := testkv.NewWithURL(t)
	smtpHost, smtpPort, mailpit := startMailpit(t)
	smtp, _ := json.Marshal(map[string]any{"host": smtpHost, "port": smtpPort, "from_address": "Akari <noreply@example.com>", "tls": "none"})
	if _, err := pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('smtp', $1)`, smtp); err != nil {
		t.Fatal(err)
	}

	// 按生产方式准备嵌入产物（DEP-01）：复制两个前端的 dist、预压缩、写入 build.json。
	// 提交取自前端构建写入的 build.json，与 make web-build 使用的提交一致。
	var built struct{ Commit string }
	raw, err := os.ReadFile(filepath.Join(web, "portal", "dist", "build.json"))
	if err != nil || json.Unmarshal(raw, &built) != nil || built.Commit == "" {
		t.Fatalf("read portal/dist/build.json (run pnpm -r build first): %v", err)
	}
	commit := built.Commit
	assets := t.TempDir()
	prep := exec.Command("go", "run", "./tools/uiprep", "-web", web, "-out", assets, "-commit", commit)
	prep.Dir = "../.."
	if out, err := prep.CombinedOutput(); err != nil {
		t.Fatalf("uiprep (run pnpm -r build first): %v\n%s", err, out)
	}

	port := freePort(t)
	portal := fmt.Sprintf("http://localhost:%d/", port)
	cfg := config.Default()
	cfg.Env = config.EnvTest
	cfg.Database.URL = dbURL
	cfg.HTTP.Listen = fmt.Sprintf("127.0.0.1:%d", port)
	cfg.Worker.Listen = ""
	cfg.HTTP.ShutdownTimeout = 5 * time.Second
	cfg.UI.Portal.PublicURL = portal
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	keys, err := secretbox.ParseKeyring(keyEnv(1), "")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := token.NewKeyring(clock.Real{}, keyEnv(2), "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := clientconfig.ParseKey(keyEnv(3))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx, app.Deps{
			Config: cfg, Log: slog.New(slog.NewTextHandler(logs, nil)), Clock: clock.Real{}, Pool: pool, KV: kvc,
			Tokens: tokens, ConfigSigner: signer, Keys: keys, Assets: os.DirFS(assets), Version: "e2e", Commit: commit,
		}, app.ModeAll)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitHealthy(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port), done)

	cmd := exec.Command("pnpm", "exec", "playwright", "test", "-c", "playwright.real.config.ts")
	cmd.Dir = web
	cmd.Env = append(os.Environ(), "PORTAL_URL="+portal, "MAILPIT_URL="+mailpit)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &out)
	cmd.Stderr = io.MultiWriter(os.Stderr, &out)
	if err := cmd.Run(); err != nil {
		t.Fatalf("playwright: %v\n--- control plane log (tail) ---\n%s", err, logs.tail(8000))
	}
}

// waitHealthy 等待 /healthz 返回 200，控制面提前退出时立即失败。
func waitHealthy(t *testing.T, url string, done <-chan error) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("control plane exited: %v", err)
		case <-deadline:
			t.Fatal("control plane did not become healthy")
		case <-tick.C:
			resp, err := http.Get(url)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return
				}
			}
		}
	}
}
