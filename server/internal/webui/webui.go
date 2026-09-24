// SPDX-License-Identifier: AGPL-3.0-or-later

// Package webui 提供内嵌的用户中心与管理后台（spec/40 DEP-01 至 DEP-06）：
// 按 Host 或路径前缀路由、SPA 回退、缓存与预压缩、安全头、在 index.html 中注入运行时配置。
//
// 每个应用挂载在“Host + 路径前缀”上；前缀之下的 v1/ 交给该应用对应的接口处理器
// （用户中心为客户端接口，管理后台为管理接口，spec/31 CON-01），其余路径为前端。
package webui

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/akari-project/panel/server/internal/config"
	"github.com/akari-project/panel/server/internal/httpx"
)

// 应用名，同时是产物中的子目录名。
const (
	Portal = "portal"
	Admin  = "admin"
)

// BuildFile 是产物中记录构建提交的文件（DEP-01）。
const BuildFile = "build.json"

// BuildInfo 是 build.json 的内容。
type BuildInfo struct {
	Commit string `json:"commit"`
}

// 校验错误。
var (
	ErrNoUI           = errors.New("webui: this binary was built without frontends; build with `make build`, or with -tags noui for a binary without UI")
	ErrCommitMismatch = errors.New("webui: embedded frontends were built from a different commit than the binary (spec/40 DEP-01)")
)

// Verify 校验嵌入产物与二进制来自同一提交，并且两个应用都存在。不一致时拒绝启动（DEP-01）。
func Verify(assets fs.FS, commit string) error {
	b, err := fs.ReadFile(assets, BuildFile)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNoUI
	}
	if err != nil {
		return fmt.Errorf("webui: read %s: %w", BuildFile, err)
	}
	var bi BuildInfo
	if err := json.Unmarshal(b, &bi); err != nil {
		return fmt.Errorf("webui: parse %s: %w", BuildFile, err)
	}
	if bi.Commit == "" || commit == "" || bi.Commit != commit {
		return fmt.Errorf("%w: frontends %q, binary %q", ErrCommitMismatch, bi.Commit, commit)
	}
	for _, app := range []string{Portal, Admin} {
		if _, err := fs.Stat(assets, app+"/index.html"); err != nil {
			return fmt.Errorf("webui: %s/index.html missing from embedded frontends: %w", app, err)
		}
	}
	return nil
}

// Options 配置路由器。
type Options struct {
	// Assets 为产物根目录（包含 portal/ 与 admin/）；nil 表示 noui 构建，前端路径返回 404。
	Assets fs.FS
	Portal config.App
	Admin  config.App
	// PortalAPI 与 AdminAPI 处理各自前缀下的 v1/ 请求，收到的路径已去掉应用前缀（以 /v1/ 开头）。
	PortalAPI http.Handler
	AdminAPI  http.Handler
	SiteName  string
	// SourceURL 是“源代码”链接（ARC-04），其中的 {commit} 替换为当前提交。
	SourceURL string
	Commit    string
	Proxies   httpx.Proxies
}

// RuntimeConfig 是注入 index.html 的 window.__PANEL_CONFIG__（DEP-04、spec/32 UI-06）。
type RuntimeConfig struct {
	App            string `json:"app"`
	SiteName       string `json:"site_name"`
	APIBaseURL     string `json:"api_base_url"`
	SourceURL      string `json:"source_url"`
	SourceRevision string `json:"source_revision"`
	CSPNonce       string `json:"csp_nonce"`
}

type mount struct {
	name string
	cfg  config.App
	api  http.Handler
	ui   *appUI // nil 表示 noui
}

// Router 按 Host 与路径前缀把请求分给两个应用。
type Router struct {
	mounts []*mount
	opts   Options
}

// New 建立路由器。Assets 非空时，预先建立两个应用的文件索引。
func New(o Options) (*Router, error) {
	rt := &Router{opts: o}
	for _, m := range []*mount{
		{name: Portal, cfg: o.Portal, api: o.PortalAPI},
		{name: Admin, cfg: o.Admin, api: o.AdminAPI},
	} {
		if m.api == nil {
			m.api = http.HandlerFunc(httpx.NotFound)
		}
		if o.Assets != nil {
			sub, err := fs.Sub(o.Assets, m.name)
			if err != nil {
				return nil, err
			}
			ui, err := newAppUI(m.name, sub)
			if err != nil {
				return nil, err
			}
			m.ui = ui
		}
		rt.mounts = append(rt.mounts, m)
	}
	return rt, nil
}

func requestHost(r *http.Request) string {
	h := r.Host
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.HasSuffix(h, "]") {
		h = h[:i]
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

// match 返回处理该请求的应用：Host 匹配（空列表匹配任意 Host）且前缀最长者；
// 前缀相同时，显式列出该 Host 的应用优先。
func (rt *Router) match(r *http.Request) *mount {
	host := requestHost(r)
	var best *mount
	bestScore := -1
	for _, m := range rt.mounts {
		explicit := false
		if len(m.cfg.Hosts) > 0 {
			for _, h := range m.cfg.Hosts {
				if h == host {
					explicit = true
				}
			}
			if !explicit {
				continue
			}
		}
		p := r.URL.Path
		if !strings.HasPrefix(p, m.cfg.PathPrefix) && p+"/" != m.cfg.PathPrefix {
			continue
		}
		score := len(m.cfg.PathPrefix) * 2
		if explicit {
			score++
		}
		if score > bestScore {
			best, bestScore = m, score
		}
	}
	return best
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m := rt.match(r)
	if m == nil {
		httpx.SetRoute(r, r.Method+" (no app)")
		httpx.NotFound(w, r)
		return
	}
	p := r.URL.Path
	if p+"/" == m.cfg.PathPrefix {
		// /console → /console/，保证前端的相对路径正确。
		httpx.SetRoute(r, r.Method+" "+m.cfg.PathPrefix)
		target := m.cfg.PathPrefix
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return
	}
	rest := strings.TrimPrefix(p, m.cfg.PathPrefix)
	if rest == "v1" || strings.HasPrefix(rest, "v1/") {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + rest
		r2.URL.RawPath = ""
		httpx.SetRoute(r, r.Method+" "+m.cfg.PathPrefix+"v1/…")
		m.api.ServeHTTP(w, r2)
		return
	}
	httpx.SetRoute(r, r.Method+" "+m.cfg.PathPrefix+"{"+m.name+"...}")
	if m.ui == nil {
		httpx.NotFound(w, r)
		return
	}
	rt.serveUI(w, r, m, rest)
}

func (rt *Router) serveUI(w http.ResponseWriter, r *http.Request, m *mount, rest string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		httpx.WriteProblem(w, r, http.StatusMethodNotAllowed, "invalid_request")
		return
	}
	nonce := newNonce()
	apiBase := rt.apiBase(r, m)
	rt.setSecurityHeaders(w, r, nonce, apiBase)

	name := path.Clean("/" + rest)[1:]
	if name == "" || name == "index.html" {
		rt.serveIndex(w, r, m, nonce, apiBase)
		return
	}
	f, ok := m.ui.files[name]
	if !ok {
		// SPA 回退：未命中静态文件时返回该应用的 index.html（DEP-02）。
		rt.serveIndex(w, r, m, nonce, apiBase)
		return
	}
	m.ui.serveFile(w, r, name, f)
}

func (rt *Router) apiBase(r *http.Request, m *mount) string {
	if m.cfg.APIBaseURL != "" {
		return strings.TrimSuffix(m.cfg.APIBaseURL, "/")
	}
	scheme := "http"
	if rt.opts.Proxies.IsTLS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + strings.TrimSuffix(m.cfg.PathPrefix, "/")
}

func origin(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// setSecurityHeaders 写出 DEP-05 规定的安全头。
func (rt *Router) setSecurityHeaders(w http.ResponseWriter, r *http.Request, nonce, apiBase string) {
	h := w.Header()
	connect := "'self'"
	if o := origin(apiBase); o != "" {
		connect += " " + o
	}
	h.Set("Content-Security-Policy", fmt.Sprintf("default-src 'self'; script-src 'self' 'nonce-%[1]s'; style-src 'self' 'nonce-%[1]s'; "+
		"img-src 'self' data:; connect-src %[2]s; frame-ancestors 'none'; base-uri 'none'; form-action 'self'", nonce, connect))
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	if rt.opts.Proxies.IsTLS(r) {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

func (rt *Router) serveIndex(w http.ResponseWriter, r *http.Request, m *mount, nonce, apiBase string) {
	cfg := RuntimeConfig{
		App:            m.name,
		SiteName:       rt.opts.SiteName,
		APIBaseURL:     apiBase,
		SourceURL:      strings.ReplaceAll(rt.opts.SourceURL, "{commit}", rt.opts.Commit),
		SourceRevision: rt.opts.Commit,
		CSPNonce:       nonce,
	}
	body := injectConfig(m.ui.index, cfg, m.cfg.PathPrefix)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}

var (
	headClose   = regexp.MustCompile(`(?i)</head\s*>`)
	nonceTags   = regexp.MustCompile(`(?i)<(script|style)\b`)
	relativeRef = regexp.MustCompile(`(?i)\b(src|href)="\./`)
)

// injectConfig 处理返回给浏览器的 index.html（嵌入约定见 web/README.md）：
//   - 给已有的 <script> 与 <style> 加上 nonce（DEP-05）；
//   - 把 src="./ 与 href="./ 改为应用挂载路径 basePath：SPA 回退的深层路径下相对路径会解析错位，
//     而 CSP 的 base-uri 'none' 不允许用 <base>。前端由入口脚本地址推出挂载路径；
//   - 在 </head> 前插入 window.__PANEL_CONFIG__（DEP-04）。JSON 编码会转义 <、>、&，不会提前结束 <script>。
func injectConfig(index []byte, cfg RuntimeConfig, basePath string) []byte {
	js, _ := json.Marshal(cfg)
	out := nonceTags.ReplaceAll(index, []byte(`<$1 nonce="`+cfg.CSPNonce+`"`))
	out = relativeRef.ReplaceAll(out, []byte(`$1="`+basePath))
	script := []byte(`<script nonce="` + cfg.CSPNonce + `">window.__PANEL_CONFIG__=` + string(js) + `;</script>`)
	if loc := headClose.FindIndex(out); loc != nil {
		return bytes.Join([][]byte{out[:loc[0]], script, out[loc[0]:]}, nil)
	}
	return append(script, out...)
}

// appUI 是一个应用的静态文件索引。
type appUI struct {
	name  string
	fsys  fs.FS
	index []byte
	files map[string]fileInfo
}

type fileInfo struct {
	etag      string
	immutable bool
	hasBr     bool
	hasGz     bool
}

// hashedAsset 匹配 Vite 默认输出的带哈希资源：assets/ 下的 name-XXXXXXXX.ext（DEP-03）。
var hashedAsset = regexp.MustCompile(`^assets/(?:.*/)?[^/]+-[A-Za-z0-9_-]{8}\.[A-Za-z0-9]+$`)

func newAppUI(name string, fsys fs.FS) (*appUI, error) {
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		return nil, fmt.Errorf("webui: %s/index.html: %w", name, err)
	}
	ui := &appUI{name: name, fsys: fsys, index: index, files: map[string]fileInfo{}}
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "index.html" {
			return err
		}
		if strings.HasSuffix(p, ".br") || strings.HasSuffix(p, ".gz") {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		fi := fileInfo{etag: hex.EncodeToString(sum[:16]), immutable: hashedAsset.MatchString(p)}
		_, errBr := fs.Stat(fsys, p+".br")
		_, errGz := fs.Stat(fsys, p+".gz")
		fi.hasBr, fi.hasGz = errBr == nil, errGz == nil
		ui.files[p] = fi
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("webui: index %s: %w", name, err)
	}
	return ui, nil
}

// serveFile 输出静态文件，按 Accept-Encoding 选择构建时预压缩的 br 或 gzip 版本。
func (ui *appUI) serveFile(w http.ResponseWriter, r *http.Request, name string, fi fileInfo) {
	h := w.Header()
	if fi.immutable {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h.Set("Content-Type", ctype)

	open, etag := name, fi.etag
	if fi.hasBr || fi.hasGz {
		h.Add("Vary", "Accept-Encoding")
		switch enc := negotiate(r.Header.Get("Accept-Encoding"), fi.hasBr, fi.hasGz); enc {
		case "br":
			open, etag = name+".br", etag+"-br"
			h.Set("Content-Encoding", "br")
		case "gzip":
			open, etag = name+".gz", etag+"-gz"
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("ETag", `"`+etag+`"`)
	f, err := ui.fsys.Open(open)
	if err != nil {
		httpx.NotFound(w, r)
		return
	}
	defer f.Close()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		b, err := io.ReadAll(f)
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusInternalServerError, "internal")
			return
		}
		rs = bytes.NewReader(b)
	}
	http.ServeContent(w, r, name, zeroTime, rs)
}

// zeroTime 让 ServeContent 不发 Last-Modified：嵌入文件没有可靠的修改时间，缓存校验只用 ETag。
var zeroTime time.Time

// negotiate 按 Accept-Encoding 选择编码，优先 br；q=0 表示拒绝。
func negotiate(accept string, hasBr, hasGz bool) string {
	ok := map[string]bool{}
	for part := range strings.SplitSeq(accept, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := strings.ReplaceAll(strings.TrimSpace(params), " ", "")
		ok[strings.ToLower(strings.TrimSpace(name))] = q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
	}
	switch {
	case hasBr && (ok["br"] || (ok["*"] && !hasKey(accept, "br"))):
		return "br"
	case hasGz && (ok["gzip"] || (ok["*"] && !hasKey(accept, "gzip"))):
		return "gzip"
	}
	return ""
}

func hasKey(accept, key string) bool {
	for part := range strings.SplitSeq(accept, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), key) {
			return true
		}
	}
	return false
}
