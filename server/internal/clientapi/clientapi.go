// SPDX-License-Identifier: AGPL-3.0-or-later

// Package clientapi 是 /v1 客户端接口（spec/30）：处理器实现 oapi-codegen 从 panel-spec 生成的
// strict server 接口（gen/），本文件负责路由与横切处理。
//
// 每个请求依次经过：
//  1. 按契约生成的操作表（gen.Operations）匹配路由，记录路由模板（CONV-23）；未实现或不存在的路由返回 404；
//  2. 请求体上限（1 MiB），超出返回 413 payload_too_large；
//  3. 认证：Bearer 或 Cookie 中的访问令牌，受众为 client，会话不在吊销集合中（AUTH-06、AUTH-08）。
//     必须认证的操作缺少或持有无效令牌时返回 401，吊销集合不可用时返回 503；
//     可选认证的操作持有无效令牌时按未认证处理；无需认证的操作（登录、刷新等）不读取令牌，
//     否则浏览器中残留的已吊销或已过期 Cookie 会让用户无法重新登录；
//  4. 限流（API-04）：无需认证的操作按 IP，已认证的写操作按账号；
//  5. Idempotency-Key（CONV-12），只用于契约声明接受该请求头的操作；
//  6. 处理器。错误一律写成 problem+json（CONV-16）。
package clientapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/akari-project/panel/server/internal/account"
	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/clientapi/gen"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/httpx"
	"github.com/akari-project/panel/server/internal/idempotency"
	"github.com/akari-project/panel/server/internal/ratelimit"
	"github.com/akari-project/panel/server/internal/session"
)

// MaxBody 是请求体上限。附件等更大的上传另行规定（CONV-33）。
const MaxBody = 1 << 20

// RequestTimeout 是单个请求的处理时限，必须短于 idempotency.Abandoned，
// 以免超时未完成的请求被当作放弃后再次执行。事件流（SSE）不受此限。
const RequestTimeout = 30 * time.Second

// AccessCookie 是用户中心的访问令牌 Cookie（AUTH-08）。
const AccessCookie = "__Host-access_token"

// 限流默认值（API-04）。
var (
	PublicRule = ratelimit.Rule{Name: "public-ip", Limit: 120, Window: time.Minute}
	WriteRule  = ratelimit.Rule{Name: "write-account", Limit: 60, Window: time.Minute}
)

// Deps 是接口所需的依赖。
type Deps struct {
	Log         *slog.Logger
	Clock       clock.Clock
	Pool        *pgxpool.Pool
	Tokens      *token.Keyring
	Revocations auth.Revocations
	Limiter     ratelimit.Limiter
	Proxies     httpx.Proxies
	// IdempotencyKey 是幂等记录中请求体摘要的 HMAC 密钥（由主密钥派生）。
	IdempotencyKey []byte
	Accounts       *account.Service
	Sessions       *session.Service
}

// Server 实现 gen.StrictServerInterface。
type Server struct {
	d Deps
}

var _ gen.StrictServerInterface = (*Server)(nil)

// New 返回 /v1 客户端接口的处理器。收到的路径以 /v1/ 开头（webui 已去掉应用前缀）。
func New(d Deps) http.Handler {
	s := &Server{d: d}
	mux := http.NewServeMux()
	fail := s.fail
	strict := gen.NewStrictHandlerWithOptions(s, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			var tooLarge *http.MaxBytesError
			switch {
			case errors.As(err, &tooLarge):
				fail(w, r, apierr.PayloadTooLarge)
			case errors.Is(err, openapi_types.ErrValidationEmail):
				// 契约中 format: email 的字段都名为 email。
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
	return &router{s: s, mux: mux, idem: idempotency.Store{Pool: d.Pool, Clock: d.Clock, HashKey: d.IdempotencyKey}}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	apierr.Write(s.d.Log, w, r, err)
}

type router struct {
	s    *Server
	mux  *http.ServeMux
	idem idempotency.Store
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, pattern := rt.mux.Handler(r)
	op, ok := gen.Operations[pattern]
	if !ok {
		rt.s.fail(w, r, apierr.NotFound)
		return
	}
	httpx.SetRoute(r, op.Pattern)
	r.Body = http.MaxBytesReader(w, r.Body, MaxBody)

	ctx := r.Context()
	if op.ID != "streamEvents" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}
	var (
		p      auth.Principal
		authed bool
	)
	ctx = withReqInfo(ctx, w, r, rt.s.d.Proxies.ClientIP(r))
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
	if op.Auth != gen.AuthPublic {
		var err error
		p, authed, err = rt.s.authenticate(r)
		switch {
		case err != nil && op.Auth == gen.AuthRequired:
			rt.s.fail(w, r, err)
			return
		case err != nil:
			authed = false // 可选认证：无效令牌按未认证处理
		case !authed && op.Auth == gen.AuthRequired:
			rt.s.fail(w, r, apierr.Unauthenticated)
			return
		}
		if authed {
			ctx = auth.WithPrincipal(ctx, p)
			r = r.WithContext(ctx)
		}
	}

	if err := rt.s.limit(ctx, r, op, authed, p); err != nil {
		rt.s.fail(w, r, err)
		return
	}

	if op.Idempotent {
		if pr, ok := auth.FromContext(ctx); ok {
			id := pr.AccountID
			rt.idem.Serve(w, r, op.Pattern, &id, rt.mux, rt.s.fail)
			return
		}
		rt.idem.Serve(w, r, op.Pattern, nil, rt.mux, rt.s.fail)
		return
	}
	rt.mux.ServeHTTP(w, r)
}

// authenticate 取出并校验访问令牌。没有令牌时 authed 为 false、err 为 nil。
// 令牌无效返回 401；吊销集合不可用返回 503，不回退为放行。
func (s *Server) authenticate(r *http.Request) (p auth.Principal, authed bool, err error) {
	raw := bearer(r)
	if raw == "" {
		if c, err := r.Cookie(AccessCookie); err == nil {
			raw = c.Value
		}
	}
	if raw == "" {
		return auth.Principal{}, false, nil
	}
	c, err := s.d.Tokens.Verify(raw, token.AudienceClient)
	if err != nil {
		return auth.Principal{}, false, apierr.Unauthenticated
	}
	revoked, err := s.d.Revocations.IsRevoked(r.Context(), c.SessionID)
	if err != nil {
		return auth.Principal{}, false, apierr.Unavailable(err)
	}
	if revoked {
		return auth.Principal{}, false, apierr.Unauthenticated
	}
	return auth.Principal{AccountID: c.AccountID, SessionID: c.SessionID, Audience: c.Audience, AMR: c.AMR}, true, nil
}

// ipSubject 是按 IP 限流的主体：IPv4 为完整地址，IPv6 为 /64（通常分配给单个用户）。
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

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// limit 执行 API-04 的通用限流：无需认证的操作按客户端 IP，已认证的写操作按账号。
// 登录、找回密码与验证码发送另有更严格的规则，由各自的处理器执行（AUTH-09）。
func (s *Server) limit(ctx context.Context, r *http.Request, op gen.Operation, authed bool, p auth.Principal) error {
	var rule ratelimit.Rule
	var subject string
	switch {
	case op.Auth == gen.AuthPublic || !authed:
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
