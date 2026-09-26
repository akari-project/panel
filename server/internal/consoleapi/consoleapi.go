// SPDX-License-Identifier: AGPL-3.0-or-later

// Package consoleapi 是管理接口（spec/31）：处理器实现 oapi-codegen 从 panel-spec 生成的
// strict server 接口（gen/），本文件负责路由与横切处理。
//
// 每个请求依次经过：
//  1. 按契约生成的操作表（gen.Operations，包括尚未实现的操作）匹配路由，记录路由模板（CONV-23）；
//     契约中不存在的路由返回 404；
//  2. 请求体上限（1 MiB），超出返回 413 payload_too_large；
//  3. 认证（无需认证的操作除外：登录、刷新、接受邀请）：Cookie 中的访问令牌，受众为 console、
//     amr 含二次验证、会话不在吊销集合中（AUTH-06、AUTH-21）；否则 401，吊销集合不可用时 503；
//  4. 管理员：账号正常、持有角色、启用二次验证，每个请求从数据库读取（AUTH-12）；否则 401；
//  5. 权限：按契约的 x-permission 判定（AUTH-17），不满足返回 403。尚未实现的操作在此之后返回 404，
//     因此每个管理接口的权限都可以测试；
//  6. 限流：无需认证的操作按 IP，已认证的写操作按账号；
//  7. If-Match：契约要求携带的操作缺少时返回 428（CONV-28），是否一致由处理器比较；
//  8. Idempotency-Key（CONV-12），只用于契约声明接受该请求头的操作；
//  9. 处理器。敏感操作（x-sensitive，AUTH-19）的处理器依次校验参数与原因（400）、Mfa-Assertion（401），
//     再执行业务，返回 401 之前不产生副作用（见 requireStepUp）。错误一律写成 problem+json（CONV-16）。
package consoleapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/audit"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/billing/catalog"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/consoleapi/gen"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/httpx"
	"github.com/akari-project/panel/server/internal/idempotency"
	"github.com/akari-project/panel/server/internal/notify"
	"github.com/akari-project/panel/server/internal/password"
	"github.com/akari-project/panel/server/internal/ratelimit"
	"github.com/akari-project/panel/server/internal/rbac"
	"github.com/akari-project/panel/server/internal/secretbox"
	"github.com/akari-project/panel/server/internal/session"
)

// MaxBody 是请求体上限。
const MaxBody = 1 << 20

// RequestTimeout 是单个请求的处理时限，必须短于 idempotency.Abandoned。
const RequestTimeout = 30 * time.Second

// 管理后台的令牌 Cookie（AUTH-08）：与用户中心的 Cookie 名不同，同主机按路径前缀部署时不会互相覆盖。
const (
	AccessCookie  = "__Host-console_access_token"
	RefreshCookie = "__Host-console_refresh_token"
)

// 限流默认值（API-04）。
var (
	PublicRule = ratelimit.Rule{Name: "console-public-ip", Limit: 120, Window: time.Minute}
	WriteRule  = ratelimit.Rule{Name: "console-write-account", Limit: 60, Window: time.Minute}
)

// Deps 是接口所需的依赖。
type Deps struct {
	Log         *slog.Logger
	Clock       clock.Clock
	Pool        *pgxpool.Pool
	Keys        *secretbox.Keyring
	Tokens      *token.Keyring
	Revocations auth.Revocations
	Limiter     ratelimit.Limiter
	Proxies     httpx.Proxies
	// IdempotencyKey 是幂等记录中请求体摘要的 HMAC 密钥（由主密钥派生）。
	IdempotencyKey []byte
	Sessions       *session.Service
	Outbox         notify.Outbox
	// Password 是新建账号（接受邀请）的密码哈希参数。
	Password password.Params
	// AdminURL 是管理后台的公开地址（以 / 结尾），用于邀请链接（AUTH-22）。
	AdminURL string
}

// Server 实现 gen.StrictServerInterface。
type Server struct {
	d       Deps
	catalog *catalog.Service
}

var _ gen.StrictServerInterface = (*Server)(nil)

// New 返回管理接口的处理器。收到的路径以 /v1/ 开头（webui 已去掉应用前缀）。
func New(d Deps) http.Handler {
	s := &Server{d: d, catalog: &catalog.Service{Pool: d.Pool, Clock: d.Clock}}
	mux := http.NewServeMux()
	fail := s.fail
	strict := gen.NewStrictHandlerWithOptions(s, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			var tooLarge *http.MaxBytesError
			switch {
			case errors.As(err, &tooLarge):
				fail(w, r, apierr.PayloadTooLarge)
			case errors.Is(err, openapi_types.ErrValidationEmail):
				fail(w, r, apierr.Invalid(apierr.Field("email", "invalid_format")))
			default:
				fail(w, r, apierr.Invalid())
			}
		},
		ResponseErrorHandlerFunc: fail,
	})
	gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{
		BaseRouter: mux,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			fail(w, r, apierr.Invalid())
		},
	})
	// all 以契约中的全部操作建立路由表，用于在权限校验之后才对尚未实现的操作返回 404。
	all := http.NewServeMux()
	for pattern := range gen.Operations {
		all.Handle(pattern, http.NotFoundHandler())
	}
	return &router{s: s, mux: mux, all: all, idem: idempotency.Store{Pool: d.Pool, Clock: d.Clock, HashKey: d.IdempotencyKey}}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	apierr.Write(s.d.Log, w, r, err)
}

type router struct {
	s    *Server
	mux  *http.ServeMux
	all  *http.ServeMux
	idem idempotency.Store
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, pattern := rt.all.Handler(r)
	op, ok := gen.Operations[pattern]
	if !ok {
		rt.s.fail(w, r, apierr.NotFound)
		return
	}
	httpx.SetRoute(r, op.Pattern)
	r.Body = http.MaxBytesReader(w, r.Body, MaxBody)

	ctx, cancel := context.WithTimeout(r.Context(), RequestTimeout)
	defer cancel()
	ip := rt.s.d.Proxies.ClientIP(r)
	ctx = withReqInfo(ctx, w, r, ip)
	ctx = audit.WithIPPrefix(ctx, httpx.IPPrefix(ip))
	r = r.WithContext(ctx)
	if rawBodyOps[op.ID] {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				rt.s.fail(w, r, apierr.PayloadTooLarge)
			} else {
				rt.s.fail(w, r, apierr.Invalid())
			}
			return
		}
		info(ctx).Body = b
		r.Body = io.NopCloser(bytes.NewReader(b))
	}

	var p auth.Principal
	authed := false
	if op.Auth != gen.AuthPublic {
		var err error
		if p, err = rt.s.authenticate(r); err != nil {
			rt.s.fail(w, r, err)
			return
		}
		staff, err := rbac.Load(ctx, sqlc.New(rt.s.d.Pool), p.AccountID)
		if errors.Is(err, rbac.ErrNotStaff) {
			rt.s.fail(w, r, apierr.Unauthenticated)
			return
		}
		if err != nil {
			rt.s.fail(w, r, err)
			return
		}
		if !staff.Allows(op.Permission) {
			rt.s.fail(w, r, apierr.Forbidden)
			return
		}
		authed = true
		ctx = withStaff(auth.WithPrincipal(ctx, p), staff)
		r = r.WithContext(ctx)
	}
	if h, pat := rt.mux.Handler(r); pat != pattern || h == nil {
		rt.s.fail(w, r, apierr.NotFound) // 契约中有、本二进制尚未实现的操作
		return
	}

	if err := rt.s.limit(ctx, r, op, authed, p); err != nil {
		rt.s.fail(w, r, err)
		return
	}
	if op.IfMatch && r.Header.Get("If-Match") == "" {
		rt.s.fail(w, r, apierr.New(http.StatusPreconditionRequired, "precondition_required"))
		return
	}
	if op.Idempotent {
		if authed {
			id := p.AccountID
			rt.idem.Serve(w, r, op.Pattern, &id, rt.mux, rt.s.fail)
			return
		}
		rt.idem.Serve(w, r, op.Pattern, nil, rt.mux, rt.s.fail)
		return
	}
	rt.mux.ServeHTTP(w, r)
}

// authenticate 校验 Cookie 中的访问令牌：受众为 console，amr 含二次验证（AUTH-21），会话未被吊销。
// 客户端接口的令牌、设备授权与扫码登录签发的令牌一律 401。吊销集合不可用返回 503，不回退为放行。
// 管理接口只接受 Cookie（契约 securitySchemes），不读取 Authorization 头。
func (s *Server) authenticate(r *http.Request) (auth.Principal, error) {
	c, err := r.Cookie(AccessCookie)
	if err != nil || c.Value == "" {
		return auth.Principal{}, apierr.Unauthenticated
	}
	claims, err := s.d.Tokens.Verify(c.Value, token.AudienceConsole)
	if err != nil || !slices.Contains(claims.AMR, "otp") {
		return auth.Principal{}, apierr.Unauthenticated
	}
	revoked, err := s.d.Revocations.IsRevoked(r.Context(), claims.SessionID)
	if err != nil {
		return auth.Principal{}, apierr.Unavailable(err)
	}
	if revoked {
		return auth.Principal{}, apierr.Unauthenticated
	}
	return auth.Principal{AccountID: claims.AccountID, SessionID: claims.SessionID, Audience: claims.Audience, AMR: claims.AMR}, nil
}

// ipSubject 是按 IP 限流的主体：IPv4 为完整地址，IPv6 为 /64。
func ipSubject(a netip.Addr) string {
	if !a.IsValid() {
		return "unknown"
	}
	if a.Is4() {
		return a.String()
	}
	pf, _ := a.Prefix(64)
	return pf.String()
}

// limit 执行通用限流：无需认证的操作按客户端 IP，已认证的写操作按账号。
// 登录与 step-up 另有更严格的规则，由会话服务执行（AUTH-09）。
func (s *Server) limit(ctx context.Context, r *http.Request, op gen.Operation, authed bool, p auth.Principal) error {
	var rule ratelimit.Rule
	var subject string
	switch {
	case !authed:
		rule, subject = PublicRule, ipSubject(s.d.Proxies.ClientIP(r))
	case op.Method != http.MethodGet:
		rule, subject = WriteRule, p.AccountID.String()
	default:
		return nil
	}
	ok, retry, err := s.d.Limiter.Allow(ctx, rule, subject)
	if err != nil {
		return apierr.Unavailable(err)
	}
	if !ok {
		return apierr.RateLimited(retry)
	}
	return nil
}
