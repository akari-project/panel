// SPDX-License-Identifier: AGPL-3.0-or-later
// 端到端测试：对构建产物（portal/dist、admin/dist）运行，Mock 网关模拟控制面的注入与安全头。
// 先运行 pnpm -r build。
import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    locale: 'zh-CN',
    timezoneId: 'Asia/Shanghai',
    trace: 'retain-on-failure',
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
