// SPDX-License-Identifier: AGPL-3.0-or-later

// Package admin 在真实控制面上运行管理后台的 Playwright 测试（backlog M1-02）。
//
// 控制面与依赖由 e2e/realpanel 启动；首个超级管理员按生产方式由命令行创建
// （panel admin create --email <邮箱> --password-stdin，spec/10 AUTH-21），尚未绑定 TOTP，
// 首次登录管理后台时必须绑定。然后运行 web/playwright.real.config.ts 中的 m1-02 测试，环境变量：
//
//	ADMIN_URL       管理后台地址，例如 http://localhost:PORT/console/（接口在 ADMIN_URL + "v1/"）
//	PORTAL_URL      用户中心地址（同一主机，路径 /）
//	MAILPIT_URL     Mailpit 的 HTTP 地址（邀请邮件、安全通知）
//	ADMIN_EMAIL     超级管理员邮箱
//	ADMIN_PASSWORD  超级管理员密码
//
// PANEL_E2E_ADMIN_SPEC 可以覆盖测试文件过滤（默认 m1-02）。
// 需要先构建前端（pnpm -r build），并设置 PANEL_E2E_PLAYWRIGHT=1；由 make e2e-admin 运行。
package admin

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akari-project/panel/server/e2e/realpanel"
)

const (
	adminEmail    = "root@example.com"
	adminPassword = "correct horse battery staple"
)

// TestM1_02_AdminPlaywright：管理员登录与首次绑定 TOTP、step-up、邀请与接受邀请、角色、审计日志（M1-02）。
func TestM1_02_AdminPlaywright(t *testing.T) {
	if os.Getenv("PANEL_E2E_PLAYWRIGHT") != "1" {
		t.Skip("set PANEL_E2E_PLAYWRIGHT=1 (make e2e-admin) to run the console Playwright suite against a real control plane")
	}
	p := realpanel.Start(t, "../../../web")
	createSuperadmin(t, p)
	checkConsoleAPI(t, p)
	spec := os.Getenv("PANEL_E2E_ADMIN_SPEC")
	if spec == "" {
		spec = "m1-02"
	}
	p.Playwright(t, []string{
		"PORTAL_URL=" + p.PortalURL, "ADMIN_URL=" + p.AdminURL, "MAILPIT_URL=" + p.MailpitURL,
		"ADMIN_EMAIL=" + adminEmail, "ADMIN_PASSWORD=" + adminPassword,
	}, spec)
}

// createSuperadmin 构建 panel 并执行 panel admin create --password-stdin（AUTH-21），与运营者的操作相同。
func createSuperadmin(t *testing.T, p *realpanel.Panel) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "panel")
	build := exec.Command("go", "build", "-tags", "noui", "-o", bin, "./cmd/panel")
	build.Dir = filepath.Join(p.Web, "..", "server")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "admin", "create", "--email", adminEmail, "--password-stdin")
	cmd.Env = append(os.Environ(), "PANEL_CONFIG=", "PANEL_DATABASE_URL="+p.DatabaseURL, "PANEL_MASTER_KEY="+p.MasterKey)
	cmd.Stdin = strings.NewReader(adminPassword + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "superadmin created") {
		t.Fatalf("panel admin create: %v\n%s", err, out)
	}
}

// checkConsoleAPI 确认管理接口挂在 ADMIN_URL + "v1/"：未登录访问 /v1/staff/me 得到 401 unauthenticated（problem+json）。
func checkConsoleAPI(t *testing.T, p *realpanel.Panel) {
	t.Helper()
	resp, err := http.Get(p.AdminURL + "v1/staff/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != 401 || body.Code != "unauthenticated" || resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("GET %sv1/staff/me: %d %q %s", p.AdminURL, resp.StatusCode, body.Code, resp.Header.Get("Content-Type"))
	}
}
