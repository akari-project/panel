// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config 加载并校验控制面配置：单一 YAML 文件加环境变量覆盖，启动时校验（spec/41 41.1）。
//
// 环境变量优先于 YAML。每个可覆盖的字段在 env 标签中声明变量名；列表类变量以逗号分隔。
// 加密主密钥只从环境变量读取，不写入 YAML（CONV-19）。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Env 是部署环境。
type Env string

// 部署环境取值。测试支付渠道只允许在 dev 与 test 中启用（spec/12 PAY-11）。
const (
	EnvDev  Env = "dev"
	EnvTest Env = "test"
	EnvProd Env = "prod"
)

// Config 是完整配置。
type Config struct {
	Env      Env      `yaml:"env" env:"PANEL_ENV"`
	Log      Log      `yaml:"log"`
	Database Database `yaml:"database"`
	Valkey   Valkey   `yaml:"valkey"`
	HTTP     HTTP     `yaml:"http"`
	Gateway  Gateway  `yaml:"gateway"`
	Worker   Worker   `yaml:"worker"`
	Site     Site     `yaml:"site"`
	UI       UI       `yaml:"ui"`
	Crypto   Crypto   `yaml:"-"`
}

// Log 是结构化日志配置（CONV-23）。
type Log struct {
	Level  string `yaml:"level" env:"PANEL_LOG_LEVEL"`   // debug、info、warn、error
	Format string `yaml:"format" env:"PANEL_LOG_FORMAT"` // json、text
}

// Database 是 PostgreSQL 连接配置。
type Database struct {
	URL      string `yaml:"url" env:"PANEL_DATABASE_URL"`
	MaxConns int32  `yaml:"max_conns" env:"PANEL_DATABASE_MAX_CONNS"`
}

// Valkey 是 Valkey 连接配置（spec/41）。api 角色必须配置。
type Valkey struct {
	// URL 形如 valkey://localhost:6379/0（也接受 redis://、rediss://）。
	URL string `yaml:"url" env:"PANEL_VALKEY_URL"`
}

// HTTP 是 api 角色（以及 all 模式下全部角色共用）的监听配置。
type HTTP struct {
	Listen string `yaml:"listen" env:"PANEL_HTTP_LISTEN"`
	// TrustedProxies 为 CIDR 列表；只有来自这些地址的请求才采信 X-Forwarded-For 与 X-Forwarded-Proto（spec/40 DEP-13）。
	TrustedProxies  []string      `yaml:"trusted_proxies" env:"PANEL_HTTP_TRUSTED_PROXIES"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" env:"PANEL_HTTP_SHUTDOWN_TIMEOUT"`

	trustedPrefixes []netip.Prefix
}

// TrustedPrefixes 返回校验后的 trusted_proxies。
func (h HTTP) TrustedPrefixes() []netip.Prefix { return h.trustedPrefixes }

// Gateway 是节点网关配置。
type Gateway struct {
	// Listen 只在单独启动 gateway 角色时使用；all 模式下与 api 共用 http.listen。
	Listen string `yaml:"listen" env:"PANEL_GATEWAY_LISTEN"`
	// Hosts 是 gateway 域名。all 模式下按 Host 把请求分给网关（spec/40 DEP-13）。
	Hosts []string `yaml:"hosts" env:"PANEL_GATEWAY_HOSTS"`
}

// Worker 是 worker 角色配置。
type Worker struct {
	// Listen 是单独启动 worker 时 /healthz 的监听地址；为空表示不监听。
	Listen string `yaml:"listen" env:"PANEL_WORKER_LISTEN"`
}

// Site 是注入前端的站点信息（spec/40 DEP-04）。
type Site struct {
	Name string `yaml:"name" env:"PANEL_SITE_NAME"`
	// SourceURL 是“源代码”链接，指向当前运行版本的仓库与提交，其中的 {commit} 替换为构建提交；
	// 运营者可改为自己的 fork（spec/01 ARC-04）。
	SourceURL string `yaml:"source_url" env:"PANEL_SITE_SOURCE_URL"`
}

// UI 是两个内嵌前端的路由配置（spec/40 DEP-02）。
type UI struct {
	Portal App `yaml:"portal"`
	Admin  App `yaml:"admin"`
}

// App 是一个前端应用的挂载方式：按 Host、路径前缀或两者组合匹配。
type App struct {
	// Hosts 为空表示匹配任意 Host。
	Hosts []string `yaml:"hosts"`
	// PathPrefix 以 / 开头并以 / 结尾。默认用户中心为 /，管理后台为 /console/。
	PathPrefix string `yaml:"path_prefix"`
	// APIBaseURL 注入前端的接口地址；为空时取请求的来源加路径前缀。
	APIBaseURL string `yaml:"api_base_url"`
}

// Crypto 是加密主密钥（CONV-19、CONV-30），只从环境变量读取。
type Crypto struct {
	// MasterKey 格式为 "<key_id>:<base64 的 32 字节密钥>"，key_id 为 1–255。
	MasterKey string `env:"PANEL_MASTER_KEY"`
	// PreviousMasterKey 为轮换期间仍需解密的旧主密钥，可为空。
	PreviousMasterKey string `env:"PANEL_MASTER_KEY_PREVIOUS"`
	// TokenKey 是访问令牌的 Ed25519 签名密钥（spec/10 AUTH-06、CONV-30），
	// 格式为 "<key_id>:<base64 的 32 字节种子>"，key_id 为 1–255。api 角色必须配置。
	TokenKey string `env:"PANEL_TOKEN_KEY"`
	// PreviousTokenKey 为轮换后仍用于验签的旧密钥，保留 30 分钟后移除，可为空。
	PreviousTokenKey string `env:"PANEL_TOKEN_KEY_PREVIOUS"`
}

// Default 返回默认配置。
func Default() Config {
	return Config{
		Env:      EnvProd,
		Log:      Log{Level: "info", Format: "json"},
		Database: Database{MaxConns: 10},
		HTTP:     HTTP{Listen: ":8080", ShutdownTimeout: 30 * time.Second},
		Gateway:  Gateway{Listen: ":8081"},
		Worker:   Worker{Listen: "127.0.0.1:8082"},
		Site:     Site{Name: "Akari", SourceURL: "https://github.com/akari-project/panel/tree/{commit}"},
		UI: UI{
			Portal: App{PathPrefix: "/"},
			Admin:  App{PathPrefix: "/console/"},
		},
	}
}

// Load 读取 path 指向的 YAML（path 为空时只用默认值），再应用环境变量并校验。
// lookup 通常为 os.LookupEnv，测试中可以替换。
func Load(path string, lookup func(string) (string, bool)) (Config, error) {
	cfg := Default()
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: %w", err)
		}
		defer f.Close()
		if err := decodeYAML(f, &cfg); err != nil {
			return Config{}, fmt.Errorf("config: %s: %w", path, err)
		}
	}
	if err := applyEnv(reflect.ValueOf(&cfg).Elem(), lookup); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func decodeYAML(r io.Reader, cfg *Config) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(cfg)
}

var durationType = reflect.TypeFor[time.Duration]()

func applyEnv(v reflect.Value, lookup func(string) (string, bool)) error {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := v.Field(i)
		name := f.Tag.Get("env")
		if name == "" {
			if fv.Kind() == reflect.Struct {
				if err := applyEnv(fv, lookup); err != nil {
					return err
				}
			}
			continue
		}
		raw, ok := lookup(name)
		if !ok {
			continue
		}
		if err := setFromString(fv, raw); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func setFromString(fv reflect.Value, raw string) error {
	switch {
	case fv.Type() == durationType:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		fv.SetInt(int64(d))
	case fv.Kind() == reflect.String:
		fv.SetString(raw)
	case fv.Kind() == reflect.Int32 || fv.Kind() == reflect.Int:
		n, err := strconv.ParseInt(raw, 10, fv.Type().Bits())
		if err != nil {
			return err
		}
		fv.SetInt(n)
	case fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() == reflect.String:
		var items []string
		for s := range strings.SplitSeq(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				items = append(items, s)
			}
		}
		fv.Set(reflect.ValueOf(items))
	default:
		return fmt.Errorf("unsupported field type %s", fv.Type())
	}
	return nil
}

// Validate 校验配置，返回全部问题。
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch c.Env {
	case EnvDev, EnvTest, EnvProd:
	default:
		add("env: must be one of dev, test, prod, got %q", c.Env)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level: must be one of debug, info, warn, error, got %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		add("log.format: must be json or text, got %q", c.Log.Format)
	}

	if c.Database.URL == "" {
		add("database.url: required (or set PANEL_DATABASE_URL)")
	} else if u, err := url.Parse(c.Database.URL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		add("database.url: must be a postgres:// URL")
	}
	if c.Valkey.URL != "" {
		if u, err := url.Parse(c.Valkey.URL); err != nil || !slices.Contains([]string{"valkey", "valkeys", "redis", "rediss", "unix"}, u.Scheme) {
			add("valkey.url: must be a valkey://, redis://, rediss:// or unix:// URL")
		}
	}
	if c.Database.MaxConns < 1 {
		add("database.max_conns: must be at least 1")
	}

	checkListen := func(field, addr string, required bool) {
		if addr == "" {
			if required {
				add("%s: required", field)
			}
			return
		}
		if _, _, err := splitHostPort(addr); err != nil {
			add("%s: %v", field, err)
		}
	}
	checkListen("http.listen", c.HTTP.Listen, true)
	checkListen("gateway.listen", c.Gateway.Listen, true)
	checkListen("worker.listen", c.Worker.Listen, false)
	if c.HTTP.ShutdownTimeout <= 0 {
		add("http.shutdown_timeout: must be positive")
	}

	c.HTTP.trustedPrefixes = c.HTTP.trustedPrefixes[:0]
	for _, s := range c.HTTP.TrustedProxies {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			add("http.trusted_proxies: %q is not a CIDR", s)
			continue
		}
		c.HTTP.trustedPrefixes = append(c.HTTP.trustedPrefixes, p.Masked())
	}

	for i, h := range c.Gateway.Hosts {
		if !validHost(h) {
			add("gateway.hosts[%d]: invalid host %q", i, h)
		}
		c.Gateway.Hosts[i] = strings.ToLower(h)
	}

	if strings.TrimSpace(c.Site.Name) == "" {
		add("site.name: required")
	}
	if u, err := url.Parse(c.Site.SourceURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		add("site.source_url: must be an absolute http(s) URL (spec/01 ARC-04)")
	}

	c.validateApp("ui.portal", &c.UI.Portal, &errs)
	c.validateApp("ui.admin", &c.UI.Admin, &errs)
	if appsOverlap(c.UI.Portal, c.UI.Admin) {
		add("ui: portal and admin match the same host and path_prefix; give them different hosts or prefixes (spec/40 DEP-02)")
	}
	for _, h := range c.Gateway.Hosts {
		if hostIn(h, c.UI.Portal.Hosts) || hostIn(h, c.UI.Admin.Hosts) {
			add("gateway.hosts: %q is also a ui host", h)
		}
	}

	return errors.Join(errs...)
}

func (c *Config) validateApp(field string, a *App, errs *[]error) {
	if a.PathPrefix == "" {
		a.PathPrefix = "/"
	}
	if !strings.HasPrefix(a.PathPrefix, "/") || !strings.HasSuffix(a.PathPrefix, "/") || strings.Contains(a.PathPrefix, "//") {
		*errs = append(*errs, fmt.Errorf("%s.path_prefix: must start and end with /, got %q", field, a.PathPrefix))
	}
	for _, reserved := range []string{"/v1/", "/healthz/"} {
		if strings.HasPrefix(a.PathPrefix, reserved) {
			*errs = append(*errs, fmt.Errorf("%s.path_prefix: %q is reserved", field, a.PathPrefix))
		}
	}
	for i, h := range a.Hosts {
		if !validHost(h) {
			*errs = append(*errs, fmt.Errorf("%s.hosts[%d]: invalid host %q", field, i, h))
		}
		a.Hosts[i] = strings.ToLower(h)
	}
	if a.APIBaseURL != "" {
		if u, err := url.Parse(a.APIBaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			*errs = append(*errs, fmt.Errorf("%s.api_base_url: must be an absolute http(s) URL", field))
		}
	}
}

// appsOverlap 判断两个应用是否会匹配同一个 Host 与同一个路径前缀。
func appsOverlap(a, b App) bool {
	if a.PathPrefix != b.PathPrefix {
		return false
	}
	if len(a.Hosts) == 0 || len(b.Hosts) == 0 {
		return true
	}
	for _, h := range a.Hosts {
		if hostIn(h, b.Hosts) {
			return true
		}
	}
	return false
}

func hostIn(h string, hosts []string) bool {
	for _, x := range hosts {
		if strings.EqualFold(h, x) {
			return true
		}
	}
	return false
}

// validHost 接受不带端口的主机名或 IP。
func validHost(h string) bool {
	if h == "" {
		return false
	}
	if _, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return true
	}
	return !strings.ContainsAny(h, "/:@ ")
}

func splitHostPort(addr string) (string, int, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("%q: missing port", addr)
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil || port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("%q: invalid port", addr)
	}
	return addr[:i], port, nil
}
