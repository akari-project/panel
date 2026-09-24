// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/clientconfig"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/config"
	"github.com/akari-project/panel/server/internal/db"
	"github.com/akari-project/panel/server/internal/httpx"
	"github.com/akari-project/panel/server/internal/secretbox"
	"github.com/akari-project/panel/server/internal/testdb"
	"github.com/akari-project/panel/server/internal/testkv"
	"github.com/akari-project/panel/server/internal/webui"
)

const testCommit = "1111111111111111111111111111111111111111"

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Database.URL = "postgres://unused"
	cfg.HTTP.Listen = "127.0.0.1:0"
	cfg.Gateway.Listen = "127.0.0.1:0"
	cfg.Worker.Listen = "127.0.0.1:0"
	cfg.HTTP.ShutdownTimeout = 5 * time.Second
	cfg.Gateway.Hosts = []string{"gateway.example.com"}
	cfg.UI.Portal.PublicURL = "https://portal.example.com/"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func testTokens(t *testing.T) *token.Keyring {
	t.Helper()
	k, err := token.NewKeyring(clock.Real{}, "1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testConfigSigner(t *testing.T) *clientconfig.Signer {
	t.Helper()
	s, err := clientconfig.ParseKey("1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testMasterKeys(t *testing.T) *secretbox.Keyring {
	t.Helper()
	k, err := secretbox.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"build.json":        {Data: []byte(`{"commit":"` + testCommit + `"}`)},
		"portal/index.html": {Data: []byte(`<html><head></head><body>portal</body></html>`)},
		"admin/index.html":  {Data: []byte(`<html><head></head><body>admin</body></html>`)},
	}
}

type started struct {
	mu    sync.Mutex
	addrs map[string]net.Addr
	ready chan struct{}
	want  int
}

func (s *started) onListen(role string, a net.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addrs[role] = a
	if len(s.addrs) == s.want {
		close(s.ready)
	}
}

// start 运行 mode，返回各监听地址与停止函数；停止函数断言 Run 正常返回。
func start(t *testing.T, d Deps, mode Mode, listeners int) (map[string]net.Addr, func()) {
	t.Helper()
	s := &started{addrs: map[string]net.Addr{}, ready: make(chan struct{}), want: listeners}
	d.OnListen = s.onListen
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, d, mode) }()
	select {
	case <-s.ready:
	case err := <-done:
		cancel()
		t.Fatalf("Run(%s) exited early: %v", mode, err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatalf("Run(%s) did not start listening", mode)
	}
	return s.addrs, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run(%s) = %v", mode, err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("Run(%s) did not stop", mode)
		}
	}
}

func get(t *testing.T, addr net.Addr, host, path string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+addr.String()+path, nil)
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func health(t *testing.T, addr net.Addr, host string) httpx.Health {
	t.Helper()
	resp, body := get(t, addr, host, "/healthz")
	if resp.StatusCode != 200 {
		t.Fatalf("/healthz = %d %s", resp.StatusCode, body)
	}
	var h httpx.Health
	if err := json.Unmarshal([]byte(body), &h); err != nil {
		t.Fatal(err)
	}
	return h
}

// M0-05 验收 3：三个角色可分别启动，也可同进程启动。
func TestRolesStartSeparatelyAndTogether(t *testing.T) {
	pool := testdb.New(t)
	d := Deps{Config: testConfig(t), Log: slog.New(slog.DiscardHandler), Clock: clock.Real{}, Pool: pool,
		KV: testkv.New(t), Tokens: testTokens(t), ConfigSigner: testConfigSigner(t), Keys: testMasterKeys(t), Assets: testAssets(),
		Version: "test", Commit: testCommit}

	for _, mode := range []Mode{ModeAPI, ModeGateway, ModeWorker} {
		t.Run(string(mode), func(t *testing.T) {
			addrs, stop := start(t, d, mode, 1)
			defer stop()
			h := health(t, addrs[string(mode)], "")
			if len(h.Roles) != 1 || h.Roles[0] != string(mode) || h.Database != "ok" {
				t.Errorf("health = %+v", h)
			}
		})
	}

	t.Run("api serves ui", func(t *testing.T) {
		addrs, stop := start(t, d, ModeAPI, 1)
		defer stop()
		resp, body := get(t, addrs["api"], "", "/")
		if resp.StatusCode != 200 || !strings.Contains(body, "portal") || !strings.Contains(body, "__PANEL_CONFIG__") {
			t.Errorf("GET / = %d %s", resp.StatusCode, body)
		}
		if _, body := get(t, addrs["api"], "", "/console/"); !strings.Contains(body, "admin") {
			t.Errorf("GET /console/ = %s", body)
		}
		if resp, _ := get(t, addrs["api"], "", "/v1/config"); resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("GET /v1/config = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if resp, _ := get(t, addrs["api"], "", "/v1/plans"); resp.StatusCode != 404 || resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Errorf("GET /v1/plans (not implemented) = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	})

	t.Run("all", func(t *testing.T) {
		addrs, stop := start(t, d, ModeAll, 1)
		defer stop()
		h := health(t, addrs["all"], "")
		if strings.Join(h.Roles, ",") != "api,gateway,worker" {
			t.Errorf("roles = %v", h.Roles)
		}
		// gateway 域名交给网关，不提供前端。
		if resp, body := get(t, addrs["all"], "gateway.example.com", "/"); resp.StatusCode != 404 || strings.Contains(body, "portal") {
			t.Errorf("gateway host / = %d %s", resp.StatusCode, body)
		}
		if _, body := get(t, addrs["all"], "example.com", "/"); !strings.Contains(body, "portal") {
			t.Errorf("api host / = %s", body)
		}
	})

	t.Run("noui", func(t *testing.T) {
		d := d
		d.Assets = nil
		d.Commit = ""
		addrs, stop := start(t, d, ModeAPI, 1)
		defer stop()
		if resp, _ := get(t, addrs["api"], "", "/"); resp.StatusCode != 404 {
			t.Errorf("noui GET / = %d", resp.StatusCode)
		}
	})
}

// DEP-01：嵌入产物与二进制提交不一致时拒绝启动；DEP-12：数据库版本过低时拒绝启动。
func TestRefusesToStart(t *testing.T) {
	pool := testdb.New(t)
	d := Deps{Config: testConfig(t), Log: slog.New(slog.DiscardHandler), Clock: clock.Real{}, Pool: pool,
		Assets: testAssets(), Version: "test", Commit: "2222222222222222222222222222222222222222"}
	if err := Run(context.Background(), d, ModeAPI); !errors.Is(err, webui.ErrCommitMismatch) {
		t.Errorf("commit mismatch: %v", err)
	}
	d.Assets = fstest.MapFS{".gitkeep": {}}
	if err := Run(context.Background(), d, ModeWorker); !errors.Is(err, webui.ErrNoUI) {
		t.Errorf("ui build without frontends: %v", err)
	}

	d.Assets = nil
	if _, err := pool.Exec(context.Background(), `DELETE FROM goose_db_version`); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), d, ModeAPI); !errors.Is(err, db.ErrSchemaTooOld) {
		t.Errorf("old schema: %v", err)
	}
}

func TestParseMode(t *testing.T) {
	for _, s := range []string{"api", "gateway", "worker", "all"} {
		if _, err := ParseMode(s); err != nil {
			t.Error(err)
		}
	}
	if _, err := ParseMode("server"); err == nil {
		t.Error("unknown mode accepted")
	}
}

// api_endpoints：主地址取 api_base_url，否则取用户中心公开地址；备用地址在后，去重（spec/30 API-11）。
func TestAPIEndpoints(t *testing.T) {
	c := config.Default()
	c.UI.Portal.PublicURL = "https://portal.example/app/"
	c.Client.APIEndpoints = []string{"https://b.example", "https://portal.example/app"}
	if got := apiEndpoints(c); strings.Join(got, " ") != "https://portal.example/app https://b.example" {
		t.Fatalf("got %v", got)
	}
	c.UI.Portal.APIBaseURL = "https://api.example"
	if got := apiEndpoints(c); strings.Join(got, " ") != "https://api.example https://b.example https://portal.example/app" {
		t.Fatalf("got %v", got)
	}
	c = config.Default()
	if got := apiEndpoints(c); len(got) != 0 {
		t.Fatalf("no addresses configured: %v", got)
	}
}
