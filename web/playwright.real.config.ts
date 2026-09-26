// SPDX-License-Identifier: AGPL-3.0-or-later
// 真实控制面上的端到端测试（M1-01 用户中心、M1-02 与 M1-04 管理后台）。不启动 webServer：
// 由 server/e2e/portal（make e2e-portal）启动 PostgreSQL、Valkey、Mailpit 与控制面后，以环境变量传入地址：
//   PORTAL_URL   用户中心地址，例如 http://localhost:8080/（__Host- Cookie 要求安全上下文，本机用 localhost）
//   MAILPIT_URL  Mailpit 的 HTTP 地址，例如 http://localhost:8025
import { defineConfig, devices } from '@playwright/test';

const portal = process.env.PORTAL_URL;
if (!portal || !process.env.MAILPIT_URL) {
  throw new Error('playwright.real.config.ts 需要环境变量 PORTAL_URL 与 MAILPIT_URL（见 make e2e-portal）');
}
const executablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE;

export default defineConfig({
  testDir: './e2e/real',
  // 共用一个控制面与 Mailpit；串行运行，避免触发登录的 IP 限流（AUTH-09）。
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  // 二次验证登录需要等到下一个 TOTP 时间步（同一时间步的验证码不能重复使用，AUTH-11）。
  timeout: 180_000,
  expect: { timeout: 15_000 },
  reporter: 'list',
  use: {
    baseURL: portal.endsWith('/') ? portal : `${portal}/`,
    locale: 'zh-CN',
    timezoneId: 'Asia/Shanghai',
    trace: 'retain-on-failure',
    ...(executablePath ? { launchOptions: { executablePath } } : {}),
  },
  projects: [{ name: 'desktop', use: { ...devices['Desktop Chrome'] } }],
});
