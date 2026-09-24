// SPDX-License-Identifier: AGPL-3.0-or-later
// 端到端测试：对构建产物（portal/dist、admin/dist）运行，Mock 网关模拟控制面的注入与安全头。
// 先运行 pnpm -r build。
import { defineConfig, devices } from '@playwright/test';

// 预装的 Chromium 与 Playwright 期望的版本不一致时（如离线环境），用该变量指定可执行文件，不运行 playwright install。
const executablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE;

export default defineConfig({
  testDir: './e2e',
  // 真实控制面上的测试由 playwright.real.config.ts 运行。
  testIgnore: ['real/**'],
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    locale: 'zh-CN',
    timezoneId: 'Asia/Shanghai',
    trace: 'retain-on-failure',
    ...(executablePath ? { launchOptions: { executablePath } } : {}),
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'] }, grepInvert: /@mobile/ },
    { name: 'mobile', use: { ...devices['Pixel 7'] }, grep: /@mobile/ },
  ],
  webServer: {
    command: 'node mock/start.mjs --serve-dist',
    url: 'http://127.0.0.1:4101/admin/',
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
  },
});
