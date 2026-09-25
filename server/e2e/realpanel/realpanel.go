// SPDX-License-Identifier: AGPL-3.0-or-later

// Package realpanel 为前端的 Playwright 测试启动真实的控制面（make e2e-portal、make e2e-admin）：
// testcontainers 启动 PostgreSQL、Valkey、Mailpit，把 web 的构建产物按生产方式准备后（DEP-01），
// 以 all 模式在进程内启动控制面（真实时钟：浏览器中的 TOTP 按真实时间计算）。
//
// 两个前端挂在同一主机（localhost，__Host- Cookie 要求安全上下文），用户中心在 /，管理后台在 /console/。
package realpanel

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

	"github.com/jackc/pgx/v5/pgxpool"
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

// Panel 是一个运行中的控制面。
type Panel struct {
	// PortalURL 与 AdminURL 是两个前端的地址（以 / 结尾）。
	PortalURL, AdminURL string
	// MailpitURL 是 Mailpit 的 HTTP 地址。
	MailpitURL string
	// DatabaseURL 与 Pool 指向本次使用的数据库。
	DatabaseURL string
	Pool        *pgxpool.Pool
	// MasterKey 是控制面使用的 PANEL_MASTER_KEY（命令行子命令须使用同一把）。
	MasterKey string
	// Web 是 web/ 目录的绝对路径。
	Web  string
	logs *syncBuffer
}

// LogTail 返回控制面日志的最后 n 字节，供失败时输出。
func (p *Panel) LogTail(n int) string { return p.logs.tail(n) }

// syncBuffer 收集控制面日志。
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

// Start 启动依赖与控制面，测试结束时停止。web 为 web/ 目录（相对路径按当前目录解析），须已构建（pnpm -r build）。
func Start(t *testing.T, web string) *Panel {
	t.Helper()
	web, err := filepath.Abs(web)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())

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
	prep.Dir = filepath.Join(web, "..", "server")
	if out, err := prep.CombinedOutput(); err != nil {
		t.Fatalf("uiprep (run pnpm -r build first): %v\n%s", err, out)
	}

	port := freePort(t)
	p := &Panel{
		PortalURL:   fmt.Sprintf("http://localhost:%d/", port),
		AdminURL:    fmt.Sprintf("http://localhost:%d/console/", port),
		MailpitURL:  mailpit,
		DatabaseURL: dbURL,
		Pool:        pool,
		MasterKey:   keyEnv(1),
		Web:         web,
		logs:        &syncBuffer{},
	}
	cfg := config.Default()
	cfg.Env = config.EnvTest
	cfg.Database.URL = dbURL
	cfg.HTTP.Listen = fmt.Sprintf("127.0.0.1:%d", port)
	cfg.Worker.Listen = ""
	cfg.HTTP.ShutdownTimeout = 5 * time.Second
	cfg.UI.Portal.PublicURL = p.PortalURL
	cfg.UI.Admin.PublicURL = p.AdminURL
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	keys, err := secretbox.ParseKeyring(p.MasterKey, "")
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
	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx, app.Deps{
			Config: cfg, Log: slog.New(slog.NewTextHandler(p.logs, nil)), Clock: clock.Real{}, Pool: pool, KV: kvc,
			Tokens: tokens, ConfigSigner: signer, Keys: keys, Assets: os.DirFS(assets), Version: "e2e", Commit: commit,
		}, app.ModeAll)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitHealthy(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port), done)
	return p
}

// Playwright 在 web/ 中运行 playwright.real.config.ts，env 追加到当前环境；args 为额外参数（如测试文件过滤）。
// 失败时输出控制面日志的末尾。
func (p *Panel) Playwright(t *testing.T, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("pnpm", append([]string{"exec", "playwright", "test", "-c", "playwright.real.config.ts"}, args...)...)
	cmd.Dir = p.Web
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &out)
	cmd.Stderr = io.MultiWriter(os.Stderr, &out)
	if err := cmd.Run(); err != nil {
		t.Fatalf("playwright: %v\n--- control plane log (tail) ---\n%s", err, p.LogTail(8000))
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
