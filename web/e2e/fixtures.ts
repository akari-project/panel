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
        // 接口返回 4xx 属于预期（未登录时 /v1/me 的 401、刷新令牌失效的 400、测试中构造的错误），
        // 浏览器会把它们记为资源加载错误；页面资源的 4xx 仍然计入。
        const expected = /status of 4\d\d/.test(m.text()) && /\/v1\//.test(m.location().url);
        if (m.type() === 'error' && !expected) errors.push(m.text());
      });
      page.on('pageerror', (e) => errors.push(String(e)));
      await use(errors);
      expect(errors, '页面不应有控制台错误或 CSP 拦截').toEqual([]);
    },
    { auto: true },
  ],
});

export { expect };
