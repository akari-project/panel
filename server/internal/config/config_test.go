// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "panel.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadYAMLAndEnvOverride(t *testing.T) {
	p := writeFile(t, `
env: dev
log: {level: debug, format: text}
database: {url: "postgres://file@localhost/panel", max_conns: 5}
http:
  listen: ":9000"
  trusted_proxies: ["10.0.0.0/8", "192.168.1.7/32"]
  shutdown_timeout: 5s
gateway: {hosts: [Gateway.Example.com]}
site: {name: Example}
ui:
  portal: {hosts: [example.com]}
  admin: {hosts: [console.example.com], path_prefix: /}
`)
	cfg, err := Load(p, env(map[string]string{
		"PANEL_DATABASE_URL":         "postgres://env@localhost/panel",
		"PANEL_DATABASE_MAX_CONNS":   "7",
		"PANEL_HTTP_TRUSTED_PROXIES": "127.0.0.1/32, ::1/128",
		"PANEL_MASTER_KEY":           "1:abc",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != EnvDev || cfg.Log.Level != "debug" || cfg.Log.Format != "text" {
		t.Errorf("env/log = %v %v", cfg.Env, cfg.Log)
	}
	if cfg.Database.URL != "postgres://env@localhost/panel" || cfg.Database.MaxConns != 7 {
		t.Errorf("database = %+v", cfg.Database)
	}
	if cfg.HTTP.Listen != ":9000" || cfg.HTTP.ShutdownTimeout != 5*time.Second {
		t.Errorf("http = %+v", cfg.HTTP)
	}
	if got := len(cfg.HTTP.TrustedPrefixes()); got != 2 {
		t.Errorf("trusted prefixes = %d, want 2 (env overrides yaml)", got)
	}
	if cfg.Gateway.Hosts[0] != "gateway.example.com" {
		t.Errorf("gateway hosts not normalised: %v", cfg.Gateway.Hosts)
	}
	if cfg.Crypto.MasterKey != "1:abc" {
		t.Error("master key not read from env")
	}
	if cfg.UI.Portal.PathPrefix != "/" {
		t.Errorf("portal prefix = %q", cfg.UI.Portal.PathPrefix)
	}
}

func TestDefaultsNeedOnlyDatabase(t *testing.T) {
	cfg, err := Load("", env(map[string]string{"PANEL_DATABASE_URL": "postgres://localhost/panel"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != EnvProd || cfg.UI.Admin.PathPrefix != "/console/" {
		t.Errorf("defaults = %+v", cfg)
	}
}

func TestValidationErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml string
		env  map[string]string
		want string
	}{
		"missing database":   {"", nil, "database.url"},
		"bad env":            {"env: staging\ndatabase: {url: postgres://x}", nil, "env:"},
		"bad level":          {"log: {level: loud}\ndatabase: {url: postgres://x}", nil, "log.level"},
		"bad cidr":           {"http: {trusted_proxies: [nope]}\ndatabase: {url: postgres://x}", nil, "trusted_proxies"},
		"bad listen":         {"http: {listen: nope}\ndatabase: {url: postgres://x}", nil, "http.listen"},
		"bad prefix":         {"ui: {admin: {path_prefix: /console}}\ndatabase: {url: postgres://x}", nil, "path_prefix"},
		"reserved prefix":    {"ui: {admin: {path_prefix: /v1/admin/}}\ndatabase: {url: postgres://x}", nil, "reserved"},
		"overlapping apps":   {"ui: {admin: {path_prefix: /}}\ndatabase: {url: postgres://x}", nil, "same host"},
		"gateway is ui host": {"gateway: {hosts: [a.example]}\nui: {portal: {hosts: [a.example]}}\ndatabase: {url: postgres://x}", nil, "also a ui host"},
		"unknown key":        {"databse: {url: postgres://x}", nil, "databse"},
		"not postgres url":   {"database: {url: mysql://x}", nil, "postgres://"},
		"bad source url":     {"site: {source_url: ftp://x}\ndatabase: {url: postgres://x}", nil, "source_url"},
		"bad env int":        {"database: {url: postgres://x}", map[string]string{"PANEL_DATABASE_MAX_CONNS": "many"}, "PANEL_DATABASE_MAX_CONNS"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeFile(t, tc.yaml), env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestAdminOnSameHostWithPrefix(t *testing.T) {
	p := writeFile(t, "database: {url: postgres://x}\nui: {portal: {hosts: [a.example]}, admin: {hosts: [a.example], path_prefix: /console/}}")
	if _, err := Load(p, env(nil)); err != nil {
		t.Fatal(err)
	}
}

// 仓库中的示例配置必须能通过校验。
func TestExampleConfig(t *testing.T) {
	if _, err := Load("../../panel.example.yaml", env(nil)); err != nil {
		t.Fatal(err)
	}
}
