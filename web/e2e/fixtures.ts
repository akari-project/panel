// SPDX-License-Identifier: AGPL-3.0-or-later
import { test as base, expect } from '@playwright/test';

export const PORTAL = 'http://127.0.0.1:4100';
export const ADMIN = 'http://127.0.0.1:4101/admin';

/** 收集控制台错误与未捕获异常（包括 CSP 拦截），测试结束时断言为空。 */
export const test = base.extend<{ pageErrors: string[] }>({
  pageErrors: [
    async ({ page }, use) => {
      const errors: string[] = [];
      page.on('console', (m) => {
        // 未登录时 /v1/me 返回 401 属于预期，浏览器会把它记为资源加载错误。
        if (m.type() === 'error' && !/status of 401/.test(m.text())) errors.push(m.text());
      });
      page.on('pageerror', (e) => errors.push(String(e)));
      await use(errors);
      expect(errors, '页面不应有控制台错误或 CSP 拦截').toEqual([]);
    },
    { auto: true },
  ],
});

export { expect };
