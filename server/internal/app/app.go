// SPDX-License-Identifier: AGPL-3.0-or-later

// Package app 组装并运行控制面的三个角色（spec/01 1.1）：api、gateway、worker，
// 可以分别启动，也可以用 all 在同一进程中启动（单机部署，spec/40 40.1）。
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/config"
	"github.com/akari-project/panel/server/internal/db"
	"github.com/akari-project/panel/server/internal/httpx"
	"github.com/akari-project/panel/server/internal/logging"
	"github.com/akari-project/panel/server/internal/webui"
)

// Mode 是启动方式。
type Mode string

// 启动方式。
const (
	ModeAPI     Mode = "api"
	ModeGateway Mode = "gateway"
	ModeWorker  Mode = "worker"
	ModeAll     Mode = "all"
)

// Roles 返回该启动方式包含的角色。
func (m Mode) Roles() []string {
	if m == ModeAll {
		return []string{string(ModeAPI), string(ModeGateway), string(ModeWorker)}
	}
	return []string{string(m)}
}

// ParseMode 解析子命令名。
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case ModeAPI, ModeGateway, ModeWorker, ModeAll:
		return m, nil
	}
	return "", fmt.Errorf("unknown role %q (want api, gateway, worker or all)", s)
}

// Deps 是运行所需的依赖。
type Deps struct {
	Config config.Config
	Log    *slog.Logger
	Clock  clock.Clock
	Pool   *pgxpool.Pool
	// Assets 为嵌入的前端产物；nil 表示 noui 构建（spec/40 DEP-06）。
	Assets  fs.FS
	Version string
	Commit  string
	// OnListen 在每个监听地址就绪后调用，可为 nil。
	OnListen func(role string, addr net.Addr)
}

// Check 执行启动前校验：数据库版本不低于二进制要求（DEP-12，不自动迁移）；
// 嵌入前端与二进制来自同一提交（DEP-01）。
func Check(ctx context.Context, d Deps) error {
	cur, req, err := db.CheckVersion(ctx, d.Pool)
	if err != nil {
		return err
	}
	d.Log.Info("database schema ok", "version", cur, "required", req)
	if d.Assets != nil {
		if err := webui.Verify(d.Assets, d.Commit); err != nil {
			return err
		}
	}
	return nil
}

// Run 启动 mode 包含的角色，直到 ctx 取消后优雅退出。
func Run(ctx context.Context, d Deps, mode Mode) error {
	if err := Check(ctx, d); err != nil {
		return err
	}
	roles := mode.Roles()
	log := d.Log.With(logging.KeyRole, string(mode))
	proxies := httpx.Proxies{Trusted: d.Config.HTTP.TrustedPrefixes()}
	health := httpx.HealthHandler(roles, d.Version, d.Pool.Ping)

	g, ctx := errgroup.WithContext(ctx)
	serve := func(role, addr string, h http.Handler) {
		g.Go(func() error { return d.serve(ctx, log, role, addr, httpx.AccessLog(log, d.Clock, h)) })
	}

	switch mode {
	case ModeAPI:
		h, err := apiHandler(d, proxies, health)
		if err != nil {
			return err
		}
		serve("api", d.Config.HTTP.Listen, h)
	case ModeGateway:
		serve("gateway", d.Config.Gateway.Listen, gatewayHandler(health))
	case ModeWorker:
		if d.Config.Worker.Listen != "" {
			serve("worker", d.Config.Worker.Listen, workerHandler(health))
		}
	case ModeAll:
		// 单机形态：一个端口，按 Host 把 gateway 域名分给网关，其余交给 api（DEP-13）。
		api, err := apiHandler(d, proxies, health)
		if err != nil {
			return err
		}
		gw := gatewayHandler(health)
		hosts := d.Config.Gateway.Hosts
		serve("all", d.Config.HTTP.Listen, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if slices.Contains(hosts, hostOnly(r.Host)) {
				gw.ServeHTTP(w, r)
				return
			}
			api.ServeHTTP(w, r)
		}))
	default:
		return fmt.Errorf("app: unknown mode %q", mode)
	}
	if mode == ModeWorker || mode == ModeAll {
		g.Go(func() error { return runWorker(ctx, d, log) })
	}
	log.Info("started", "roles", roles, "version", d.Version, "commit", d.Commit, "ui", d.Assets != nil)
	err := g.Wait()
	log.Info("stopped")
	return err
}

func hostOnly(h string) string {
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.HasSuffix(h, "]") {
		h = h[:i]
	}
	return strings.ToLower(h)
}

func (d Deps) serve(ctx context.Context, log *slog.Logger, role, addr string, h http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%s: listen %s: %w", role, addr, err)
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	log.Info("listening", "listener", role, "addr", ln.Addr().String())
	if d.OnListen != nil {
		d.OnListen(role, ln.Addr())
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("%s: serve: %w", role, err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.Config.HTTP.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("%s: shutdown: %w", role, err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// apiHandler 是 api 角色：/healthz、两个前端，以及各自前缀下的客户端接口与管理接口。
// 接口本身在 M1 实现（spec/30、spec/31），目前一律返回 404 not_found。
func apiHandler(d Deps, proxies httpx.Proxies, health http.Handler) (http.Handler, error) {
	ui, err := webui.New(webui.Options{
		Assets:    d.Assets,
		Portal:    d.Config.UI.Portal,
		Admin:     d.Config.UI.Admin,
		PortalAPI: http.HandlerFunc(httpx.NotFound),
		AdminAPI:  http.HandlerFunc(httpx.NotFound),
		SiteName:  d.Config.Site.Name,
		SourceURL: d.Config.Site.SourceURL,
		Commit:    d.Commit,
		Proxies:   proxies,
	})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	httpx.Handle(mux, "GET /healthz", health)
	mux.Handle("/", ui)
	return mux, nil
}

// gatewayHandler 是 gateway 角色：/healthz；节点接入与长连接在 M2 实现（spec/20）。
func gatewayHandler(health http.Handler) http.Handler {
	mux := http.NewServeMux()
	httpx.Handle(mux, "GET /healthz", health)
	httpx.Handle(mux, "/", http.HandlerFunc(httpx.NotFound))
	return mux
}

// workerHandler 只提供 /healthz。
func workerHandler(health http.Handler) http.Handler {
	mux := http.NewServeMux()
	httpx.Handle(mux, "GET /healthz", health)
	httpx.Handle(mux, "/", http.HandlerFunc(httpx.NotFound))
	return mux
}

// runWorker 是 worker 角色的主循环。周期任务（spec/01 1.1）随各里程碑加入；
// 多实例下的单执行者选举见 spec/40 DEP-08。
func runWorker(ctx context.Context, _ Deps, log *slog.Logger) error {
	log.Info("worker running", "jobs", 0)
	<-ctx.Done()
	return nil
}
