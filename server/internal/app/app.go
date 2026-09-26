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
	"github.com/valkey-io/valkey-go"
	"golang.org/x/sync/errgroup"

	"github.com/akari-project/panel/server/internal/account"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/clientapi"
	"github.com/akari-project/panel/server/internal/clientconfig"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/config"
	"github.com/akari-project/panel/server/internal/consoleapi"
	"github.com/akari-project/panel/server/internal/db"
	"github.com/akari-project/panel/server/internal/httpx"
	"github.com/akari-project/panel/server/internal/idempotency"
	"github.com/akari-project/panel/server/internal/logging"
	"github.com/akari-project/panel/server/internal/mfa"
	"github.com/akari-project/panel/server/internal/notify"
	"github.com/akari-project/panel/server/internal/password"
	"github.com/akari-project/panel/server/internal/ratelimit"
	"github.com/akari-project/panel/server/internal/secretbox"
	"github.com/akari-project/panel/server/internal/session"
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
	// KV 与 Tokens 是 api 角色所需的 Valkey 客户端与访问令牌密钥环；Keys 是主密钥环，
	// api 与 worker 角色需要。只启动 gateway 时都可为 nil。
	KV     valkey.Client
	Tokens *token.Keyring
	Keys   *secretbox.Keyring
	// ConfigSigner 是 /v1/config 的签名密钥（CONV-30 PANEL_CONFIG_KEY），api 角色需要。
	ConfigSigner *clientconfig.Signer
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
func apiHandler(d Deps, proxies httpx.Proxies, health http.Handler) (http.Handler, error) {
	if d.KV == nil || d.Tokens == nil || d.Keys == nil {
		return nil, errors.New("api: valkey.url, PANEL_TOKEN_KEY and PANEL_MASTER_KEY are required")
	}
	portalURL := d.Config.UI.Portal.URL()
	if portalURL == "" {
		return nil, errors.New("api: ui.portal.public_url is required when ui.portal.hosts is not a single host (links in emails, spec/10 AUTH-04)")
	}
	limiter := ratelimit.Limiter{KV: d.KV}
	revocations := auth.Revocations{KV: d.KV}
	outbox := notify.Outbox{Keys: d.Keys, Clock: d.Clock}
	sessions := &session.Service{
		Pool: d.Pool, KV: d.KV, Clock: d.Clock, Keys: d.Keys, Tokens: d.Tokens, Revocations: revocations, Limiter: limiter,
		Outbox: outbox, Log: d.Log,
	}
	sessions.MFA = &mfa.Service{
		Pool: d.Pool, KV: d.KV, Clock: d.Clock, Keys: d.Keys, Outbox: outbox, Issuer: d.Config.Site.Name,
		AfterRevoke: sessions.AfterRevoke,
	}
	accounts := &account.Service{
		Pool: d.Pool, Clock: d.Clock, Keys: d.Keys, Outbox: outbox, Limiter: limiter,
		Password: password.DefaultParams, Invites: account.ReferralCodes{}, Captcha: account.NoCaptcha{},
		PortalURL: portalURL, Revoke: sessions.RevokeAccount, AfterRevoke: sessions.AfterRevoke, Log: d.Log,
	}
	client := clientapi.New(clientapi.Deps{
		Log:            d.Log,
		Clock:          d.Clock,
		Pool:           d.Pool,
		Tokens:         d.Tokens,
		Revocations:    revocations,
		Limiter:        limiter,
		Accounts:       accounts,
		Sessions:       sessions,
		MFA:            sessions.MFA,
		Proxies:        proxies,
		IdempotencyKey: d.Keys.Derive("idempotency-request-hash"),
		Config: clientapi.ConfigDeps{
			Signer:       d.ConfigSigner,
			AppName:      d.Config.Client.AppName,
			APIEndpoints: apiEndpoints(d.Config),
		},
		ExportBaseURL: primaryAPI(d.Config),
	})
	adminURL := d.Config.UI.Admin.URL()
	if adminURL == "" {
		d.Log.Warn("ui.admin.public_url is not set and ui.admin.hosts is not a single host: staff invitations are unavailable (spec/10 AUTH-22)")
	}
	console := consoleapi.New(consoleapi.Deps{
		Log:            d.Log,
		Clock:          d.Clock,
		Pool:           d.Pool,
		Keys:           d.Keys,
		Tokens:         d.Tokens,
		Revocations:    revocations,
		Limiter:        limiter,
		Proxies:        proxies,
		IdempotencyKey: d.Keys.Derive("idempotency-request-hash"),
		Sessions:       sessions,
		Outbox:         outbox,
		Password:       password.DefaultParams,
		AdminURL:       adminURL,
	})
	ui, err := webui.New(webui.Options{
		Assets:    d.Assets,
		Portal:    d.Config.UI.Portal,
		Admin:     d.Config.UI.Admin,
		PortalAPI: client,
		AdminAPI:  console,
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
// 多实例下的单执行者选举见 spec/40 DEP-08。目前的任务都可以由多个实例重复执行。
func runWorker(ctx context.Context, d Deps, log *slog.Logger) error {
	if d.Keys == nil {
		return errors.New("worker: PANEL_MASTER_KEY is required")
	}
	idem := idempotency.Store{Pool: d.Pool, Clock: d.Clock}
	deliverer := &notify.Deliverer{
		Pool: d.Pool, Keys: d.Keys, Clock: d.Clock, Log: log, SiteName: d.Config.Site.Name,
		Sender: notify.SMTPSender{Pool: d.Pool, Keys: d.Keys, Clock: d.Clock},
	}
	jobs := []job{
		// 外发通知（spec/13 OPS-02）：多个 worker 并行时以 SKIP LOCKED 分摊。
		{name: "notification-delivery", every: 5 * time.Second, run: func(ctx context.Context) error {
			_, err := deliverer.RunOnce(ctx)
			return err
		}},
		{name: "idempotency-sweep", every: time.Hour, run: func(ctx context.Context) error {
			n, err := idem.Sweep(ctx)
			if err == nil && n > 0 {
				log.Info("idempotency keys swept", "rows", n)
			}
			return err
		}},
	}
	log.Info("worker running", "jobs", len(jobs))
	g, ctx := errgroup.WithContext(ctx)
	for _, j := range jobs {
		g.Go(func() error {
			t := time.NewTicker(j.every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-t.C:
					if err := j.run(ctx); err != nil && ctx.Err() == nil {
						log.Warn("job failed", "job", j.name, "error", err)
					}
				}
			}
		})
	}
	return g.Wait()
}

type job struct {
	name  string
	every time.Duration
	run   func(context.Context) error
}

// apiEndpoints 是 /v1/config 的 api_endpoints（spec/30 API-11）：主地址取 ui.portal.api_base_url，
// 未配置时取用户中心的公开地址；其后为 client.api_endpoints 中的备用地址，去重并保持顺序。
func apiEndpoints(c config.Config) []string {
	out := []string{}
	for _, e := range append([]string{primaryAPI(c)}, c.Client.APIEndpoints...) {
		e = strings.TrimSuffix(e, "/")
		if e != "" && !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

// primaryAPI 是接口主地址（spec/30 API-11）：ui.portal.api_base_url，未配置时取用户中心的公开地址，去掉末尾的 /。
// 导入链接同样以它为前缀（AUTH-16）。
func primaryAPI(c config.Config) string {
	primary := c.UI.Portal.APIBaseURL
	if primary == "" {
		primary = c.UI.Portal.URL()
	}
	return strings.TrimSuffix(primary, "/")
}
