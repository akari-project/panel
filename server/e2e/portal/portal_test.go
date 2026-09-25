// SPDX-License-Identifier: AGPL-3.0-or-later

// Package portal 在真实控制面上运行用户中心的 Playwright 测试（backlog M1-01 验收 4）。
//
// 控制面与依赖由 e2e/realpanel 启动；运行 web/playwright.real.config.ts 中的 m1-01 测试，
// 环境变量 PORTAL_URL、ADMIN_URL 与 MAILPIT_URL 指向本次启动的服务。
//
// 需要先构建前端（pnpm -r build），并设置 PANEL_E2E_PLAYWRIGHT=1；由 make e2e-portal 运行。
package portal

import (
	"os"
	"testing"

	"github.com/akari-project/panel/server/e2e/realpanel"
)

// TestM1_01_PortalPlaywright：注册、邮箱验证、登录、启用 TOTP 与二次验证登录、找回密码（M1-01 验收 4）。
func TestM1_01_PortalPlaywright(t *testing.T) {
	if os.Getenv("PANEL_E2E_PLAYWRIGHT") != "1" {
		t.Skip("set PANEL_E2E_PLAYWRIGHT=1 (make e2e-portal) to run the portal Playwright suite against a real control plane")
	}
	p := realpanel.Start(t, "../../../web")
	p.Playwright(t, []string{"PORTAL_URL=" + p.PortalURL, "ADMIN_URL=" + p.AdminURL, "MAILPIT_URL=" + p.MailpitURL}, "m1-01")
}
