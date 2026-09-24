// SPDX-License-Identifier: AGPL-3.0-or-later

package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/akari-project/panel/server/internal/config"
	"github.com/akari-project/panel/server/internal/httpx"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

// placeholderIndex 模拟 Vite 产物：相对路径的模块脚本与样式。
const placeholderIndex = `<!doctype html><html><head><meta charset="utf-8"><title>x</title>` +
	`<script type="module" crossorigin src="./assets/index-AbC12_-9.js"></script>` +
	`<link rel="stylesheet" href="./assets/index-ZZZZZZZZ.css"><style>body{}</style></head><body><div id="root"></div></body></html>`

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"build.json":                         {Data: []byte(`{"commit":"` + commit + `"}`)},
		"portal/index.html":                  {Data: []byte(placeholderIndex)},
		"portal/assets/index-AbC12_-9.js":    {Data: []byte("console.log('portal')")},
		"portal/assets/index-AbC12_-9.js.br": {Data: []byte("BR")},
		"portal/assets/index-AbC12_-9.js.gz": {Data: []byte("GZ")},
		"portal/favicon.ico":                 {Data: []byte("ico")},
		"admin/index.html":                   {Data: []byte(strings.Replace(placeholderIndex, "<title>x", "<title>admin", 1))},
		"admin/assets/index-QQQQQQQQ.js":     {Data: []byte("console.log('admin')")},
	}
}

type apiStub string

func (s apiStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-API", string(s))
	_, _ = io.WriteString(w, r.URL.Path)
}

func newRouter(t *testing.T, portal, admin config.App, assets fstest.MapFS) *Router {
	t.Helper()
	opts := Options{
		Portal: portal, Admin: admin,
		PortalAPI: apiStub("client"), AdminAPI: apiStub("console"),
		SiteName: "Akari <Test>", SourceURL: "https://example.org/panel/tree/{commit}", Commit: commit,
		Proxies: httpx.Proxies{Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}},
	}
	if assets != nil {
		opts.Assets = assets
	}
	rt, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func do(h http.Handler, method, host, target string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	r.RemoteAddr = "203.0.113.9:5555"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var configRe = regexp.MustCompile(`<script nonce="([^"]+)">window\.__PANEL_CONFIG__=(\{.*?\});</script>`)

func runtimeConfig(t *testing.T, body string) (RuntimeConfig, string) {
	t.Helper()
	m := configRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no injected config in %s", body)
	}
	var rc RuntimeConfig
	if err := json.Unmarshal([]byte(m[2]), &rc); err != nil {
		t.Fatal(err)
	}
	return rc, m[1]
}

// DEP-02：按 Host 与路径前缀路由；前缀之下的 v1/ 交给对应接口。
func TestRoutingByHostAndPrefix(t *testing.T) {
	rt := newRouter(t,
		config.App{Hosts: []string{"example.com"}, PathPrefix: "/"},
		config.App{Hosts: []string{"console.example.com", "example.com"}, PathPrefix: "/console/"},
		testAssets())

	for _, tc := range []struct {
		host, path, wantTitle, wantAPI, wantAPIPath string
		status                                      int
	}{
		{"example.com", "/", "<title>x", "", "", 200},
		{"example.com", "/plans/abc", "<title>x", "", "", 200}, // SPA 回退
		{"example.com", "/console/", "<title>admin", "", "", 200},
		{"example.com", "/console/users/1", "<title>admin", "", "", 200},
		{"console.example.com", "/console/", "<title>admin", "", "", 200},
		{"example.com:8443", "/", "<title>x", "", "", 200},
		{"example.com", "/v1/config", "", "client", "/v1/config", 200},
		{"example.com", "/console/v1/staff/me", "", "console", "/v1/staff/me", 200},
		{"other.example", "/", "", "", "", 404},
	} {
		t.Run(tc.host+tc.path, func(t *testing.T) {
			w := do(rt, "GET", tc.host, tc.path, nil)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
			if tc.wantTitle != "" && !strings.Contains(w.Body.String(), tc.wantTitle) {
				t.Errorf("body = %s", w.Body.String())
			}
			if w.Header().Get("X-API") != tc.wantAPI {
				t.Errorf("api = %q, want %q", w.Header().Get("X-API"), tc.wantAPI)
			}
			if tc.wantAPIPath != "" && w.Body.String() != tc.wantAPIPath {
				t.Errorf("api path = %q, want %q", w.Body.String(), tc.wantAPIPath)
			}
		})
	}

	w := do(rt, "GET", "example.com", "/console?x=1", nil)
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/console/?x=1" {
		t.Errorf("prefix without slash: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestSeparateHostsSamePrefix(t *testing.T) {
	rt := newRouter(t,
		config.App{Hosts: []string{"example.com"}, PathPrefix: "/"},
		config.App{Hosts: []string{"console.example.com"}, PathPrefix: "/"},
		testAssets())
	if w := do(rt, "GET", "console.example.com", "/users", nil); !strings.Contains(w.Body.String(), "<title>admin") {
		t.Errorf("console host served %s", w.Body.String())
	}
	if w := do(rt, "GET", "console.example.com", "/v1/staff/me", nil); w.Header().Get("X-API") != "console" {
		t.Error("console host /v1 not routed to console API")
	}
	if w := do(rt, "GET", "example.com", "/users", nil); !strings.Contains(w.Body.String(), "<title>x") {
		t.Errorf("portal host served %s", w.Body.String())
	}
}

// DEP-04、DEP-05：注入运行时配置与 nonce，并返回安全头。
func TestIndexInjectionAndSecurityHeaders(t *testing.T) {
	rt := newRouter(t, config.App{PathPrefix: "/"}, config.App{PathPrefix: "/console/", APIBaseURL: "https://api.example.com/console/"}, testAssets())

	w := do(rt, "GET", "example.com", "/", nil)
	body := w.Body.String()
	rc, nonce := runtimeConfig(t, body)
	want := RuntimeConfig{App: "portal", SiteName: "Akari <Test>", APIBaseURL: "http://example.com",
		SourceURL: "https://example.org/panel/tree/" + commit, SourceRevision: commit, CSPNonce: nonce}
	if rc != want {
		t.Errorf("config = %+v\nwant %+v", rc, want)
	}
	if strings.Contains(body, "Akari <Test>") {
		t.Error("site name not HTML-escaped inside <script>")
	}
	if n := strings.Count(body, `nonce="`+nonce+`"`); n != 3 { // 注入脚本、模块脚本、内联样式
		t.Errorf("nonce attributes = %d, want 3: %s", n, body)
	}
	if i := strings.Index(body, "__PANEL_CONFIG__"); i < 0 || i > strings.Index(body, "</head>") {
		t.Error("config must be injected inside <head>, before </head>")
	}
	if strings.Contains(body, "base_path") {
		t.Error("base_path is not part of __PANEL_CONFIG__ (web/README.md)")
	}

	h := w.Header()
	csp := h.Get("Content-Security-Policy")
	wantCSP := "default-src 'self'; script-src 'self' 'nonce-" + nonce + "'; style-src 'self' 'nonce-" + nonce + "'; img-src 'self' data:; " +
		"connect-src 'self' http://example.com; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
	if csp != wantCSP {
		t.Errorf("CSP = %s\nwant  %s", csp, wantCSP)
	}
	for k, v := range map[string]string{
		"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff",
		"Referrer-Policy": "strict-origin-when-cross-origin", "Cache-Control": "no-cache",
		"Content-Type": "text/html; charset=utf-8",
	} {
		if h.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, h.Get(k), v)
		}
	}
	if h.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS sent over plain HTTP")
	}

	// 每次响应的 nonce 不同。
	_, nonce2 := runtimeConfig(t, do(rt, "GET", "example.com", "/", nil).Body.String())
	if nonce2 == nonce {
		t.Error("nonce reused across responses")
	}

	// 显式配置的接口地址；通过可信代理的 TLS 请求带 HSTS。
	r := httptest.NewRequest("GET", "/console/", nil)
	r.Host = "example.com"
	r.RemoteAddr = "10.1.2.3:4444"
	r.Header.Set("X-Forwarded-Proto", "https")
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	rc, _ = runtimeConfig(t, w.Body.String())
	if rc.App != "admin" || rc.APIBaseURL != "https://api.example.com/console" {
		t.Errorf("admin config = %+v", rc)
	}
	if w.Header().Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Error("HSTS missing over TLS")
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "connect-src 'self' https://api.example.com;") {
		t.Errorf("CSP connect-src = %s", w.Header().Get("Content-Security-Policy"))
	}

	// 未经可信代理的 X-Forwarded-Proto 不被采信（DEP-13）。
	w = do(rt, "GET", "example.com", "/", map[string]string{"X-Forwarded-Proto": "https"})
	if w.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS from untrusted X-Forwarded-Proto")
	}
}

// DEP-03：带哈希资源长期缓存，其余 no-cache；按 Accept-Encoding 提供预压缩版本。
func TestStaticCachingAndPrecompression(t *testing.T) {
	rt := newRouter(t, config.App{PathPrefix: "/"}, config.App{PathPrefix: "/console/"}, testAssets())

	w := do(rt, "GET", "example.com", "/assets/index-AbC12_-9.js", map[string]string{"Accept-Encoding": "gzip, br"})
	if w.Body.String() != "BR" || w.Header().Get("Content-Encoding") != "br" {
		t.Errorf("br: %q %q", w.Body.String(), w.Header().Get("Content-Encoding"))
	}
	if w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") || w.Header().Get("Vary") != "Accept-Encoding" {
		t.Errorf("type %q vary %q", w.Header().Get("Content-Type"), w.Header().Get("Vary"))
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Security-Policy") == "" {
		t.Error("security headers missing on static asset")
	}
	etagBr := w.Header().Get("ETag")

	w = do(rt, "GET", "example.com", "/assets/index-AbC12_-9.js", map[string]string{"Accept-Encoding": "gzip"})
	if w.Body.String() != "GZ" || w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("gzip: %q %q", w.Body.String(), w.Header().Get("Content-Encoding"))
	}
	w = do(rt, "GET", "example.com", "/assets/index-AbC12_-9.js", map[string]string{"Accept-Encoding": "br;q=0, identity"})
	if w.Body.String() != "console.log('portal')" || w.Header().Get("Content-Encoding") != "" {
		t.Errorf("identity: %q %q", w.Body.String(), w.Header().Get("Content-Encoding"))
	}
	if w.Header().Get("ETag") == etagBr {
		t.Error("encoded and identity variants share an ETag")
	}

	w = do(rt, "GET", "example.com", "/favicon.ico", nil)
	if w.Header().Get("Cache-Control") != "no-cache" || w.Body.String() != "ico" {
		t.Errorf("favicon: %q %q", w.Header().Get("Cache-Control"), w.Body.String())
	}
	w2 := do(rt, "GET", "example.com", "/favicon.ico", map[string]string{"If-None-Match": w.Header().Get("ETag")})
	if w2.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", w2.Code)
	}

	if w := do(rt, "POST", "example.com", "/", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
	if w := do(rt, "GET", "example.com", "/../../build.json", nil); strings.Contains(w.Body.String(), commit+`"}`) && !strings.Contains(w.Body.String(), "__PANEL_CONFIG__") {
		t.Error("path traversal served build.json")
	}
	if w := do(rt, "HEAD", "example.com", "/", nil); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD: %d len %d", w.Code, w.Body.Len())
	}
}

// DEP-06：noui 构建中前端路径返回 404，接口照常。
func TestNoUI(t *testing.T) {
	rt := newRouter(t, config.App{PathPrefix: "/"}, config.App{PathPrefix: "/console/"}, nil)
	if w := do(rt, "GET", "example.com", "/", nil); w.Code != 404 {
		t.Errorf("ui path: %d", w.Code)
	}
	if w := do(rt, "GET", "example.com", "/v1/config", nil); w.Header().Get("X-API") != "client" {
		t.Error("api not routed without ui")
	}
}

// DEP-01：嵌入产物的提交与二进制不一致时拒绝启动。
func TestVerify(t *testing.T) {
	if err := Verify(testAssets(), commit); err != nil {
		t.Fatalf("matching commit: %v", err)
	}
	if err := Verify(testAssets(), "fedcba"); !errors.Is(err, ErrCommitMismatch) {
		t.Errorf("mismatch: %v", err)
	}
	if err := Verify(testAssets(), ""); !errors.Is(err, ErrCommitMismatch) {
		t.Errorf("binary without commit: %v", err)
	}
	if err := Verify(fstest.MapFS{".gitkeep": {}}, commit); !errors.Is(err, ErrNoUI) {
		t.Errorf("empty dist: %v", err)
	}
	a := testAssets()
	delete(a, "admin/index.html")
	if err := Verify(a, commit); err == nil {
		t.Error("missing admin accepted")
	}
}

// 深层 SPA 路径下，./ 开头的资源引用改为挂载路径，与注入配置、补 nonce 在同一步完成。
func TestDeepRouteFallbackRewritesRelativePaths(t *testing.T) {
	a := testAssets()
	a["admin/index.html"] = &fstest.MapFile{Data: []byte(`<!doctype html><html><head><meta charset="utf-8">` +
		`<script src="./assets/theme-init-0a1b2c3d.js"></script>` +
		`<script type="module" src="./assets/index-QQQQQQQQ.js"></script><link rel="icon" href="./favicon.ico">` +
		`<link rel="preconnect" href="https://cdn.example"></head><body><a href="./x">x</a></body></html>`)}
	a["portal/index.html"] = &fstest.MapFile{Data: []byte(`<html><head><script type="module" src="./assets/index-AbC12_-9.js"></script></head></html>`)}
	rt := newRouter(t, config.App{PathPrefix: "/"}, config.App{PathPrefix: "/console/"}, a)

	for _, tc := range []struct{ path, prefix string }{
		{"/console/users/123/devices", "/console/"},
		{"/console/", "/console/"},
		{"/plans/basic/checkout", "/"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := do(rt, "GET", "example.com", tc.path, nil)
			body := w.Body.String()
			if w.Code != 200 || strings.Contains(body, `"./`) {
				t.Fatalf("%d, relative reference left: %s", w.Code, body)
			}
			_, nonce := runtimeConfig(t, body)
			if tc.prefix == "/console/" {
				for _, want := range []string{
					`<script nonce="` + nonce + `" src="/console/assets/theme-init-0a1b2c3d.js">`,
					`<script nonce="` + nonce + `" type="module" src="/console/assets/index-QQQQQQQQ.js">`,
					`href="/console/favicon.ico"`, `href="/console/x"`, `href="https://cdn.example"`,
				} {
					if !strings.Contains(body, want) {
						t.Errorf("missing %s in %s", want, body)
					}
				}
			} else if !strings.Contains(body, `src="/assets/index-AbC12_-9.js"`) {
				t.Errorf("portal not rewritten: %s", body)
			}
		})
	}
}

// 带哈希的资源名可以含 - 与 _（base64url）。
func TestHashedAssetPattern(t *testing.T) {
	for name, want := range map[string]bool{
		"assets/index-AbC12_-9.js":            true,
		"assets/rolldown-runtime-CbXtAM7H.js": true,
		"assets/theme-init-0a1b2c3d.js":       true,
		"assets/fonts/inter-Q_q-1234.woff2":   true,
		"assets/logo.svg":                     false,
		"favicon-AbCdEf12.ico":                false,
		"assets/apple-touch-icon.png":         false,
	} {
		if got := hashedAsset.MatchString(name); got != want {
			t.Errorf("%s: hashed = %v, want %v", name, got, want)
		}
	}
}
