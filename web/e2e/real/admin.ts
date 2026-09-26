// SPDX-License-Identifier: AGPL-3.0-or-later
// 真实控制面上管理后台测试的共用步骤（M1-02、M1-04）：首次登录绑定 TOTP、敏感操作的重新验证、收集控制台错误。
import { expect, type Browser, type Page } from '@playwright/test';
import { freshTotp } from './totp';

export const ADMIN = (() => {
  const u = process.env.ADMIN_URL ?? '';
  return u.endsWith('/') ? u : `${u}/`;
})();
export const ADMIN_EMAIL = process.env.ADMIN_EMAIL ?? '';
export const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD ?? '';

// 接口的 4xx（未登录时的 /v1/staff/me、第一步登录的 mfa_required、测试构造的错误）属于预期；其余控制台错误使测试失败。
export function collectPageErrors(page: Page) {
  const errors: string[] = [];
  page.on('console', (m) => {
    const expected = /status of 4\d\d/.test(m.text()) && /\/v1\//.test(m.location().url);
    if (m.type() === 'error' && !expected) errors.push(m.text());
  });
  page.on('pageerror', (e) => errors.push(String(e)));
  return errors;
}

export interface Totp {
  secret: string;
  step: number;
}

export async function newPage(browser: Browser) {
  const page = await (await browser.newContext()).newPage();
  return { page, errors: collectPageErrors(page) };
}

/** 首次登录：提交密码 → 绑定 TOTP（读取页面上的密钥）→ 保存恢复码 → 进入概览（AUTH-21）。 */
export async function firstSignIn(page: Page, email: string, password: string): Promise<Totp> {
  await page.goto(`${ADMIN}login`);
  await page.getByLabel('邮箱').fill(email);
  await page.getByLabel('密码').fill(password);
  await page.getByRole('button', { name: '登录' }).click();
  await expect(page.getByRole('img', { name: '绑定身份验证器的二维码' })).toBeVisible();
  const secret = (await page.locator('code').first().textContent())!.replace(/\s/g, '');
  expect(secret).toMatch(/^[A-Z2-7]+=*$/);
  const { code, step } = await freshTotp(secret);
  await page.getByLabel('验证码').fill(code);
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await expect(page.getByRole('heading', { name: '请保存恢复码' })).toBeVisible();
  await expect(page.getByRole('list', { name: '恢复码' }).getByRole('listitem')).toHaveCount(10);
  await page.getByRole('button', { name: '我已保存' }).click();
  await expect(page.getByRole('heading', { name: '概览', level: 1 })).toBeVisible();
  return { secret, step };
}

/**
 * 点击敏感操作的提交按钮后：若弹出重新验证框则用新的 TOTP 完成（AUTH-19），然后等待 done 出现。
 * 5 分钟内复用断言时不会弹出验证框。
 */
export async function submitSensitive(page: Page, totp: Totp, submit: () => Promise<void>, done: () => Promise<void>) {
  await submit();
  const stepUp = page.getByRole('dialog', { name: '验证身份' });
  // 两个分支中未胜出的一方稍后会超时失败，吞掉其错误，避免未处理的拒绝。
  const outcome = await Promise.race([
    stepUp.waitFor({ state: 'visible', timeout: 60_000 }).then(
      () => 'step-up' as const,
      () => 'timeout' as const,
    ),
    done().then(
      () => 'done' as const,
      () => 'timeout' as const,
    ),
  ]);
  if (outcome === 'done') return;
  if (outcome === 'timeout') throw new Error('既没有弹出重新验证框，也没有出现预期结果');
  const { code, step } = await freshTotp(totp.secret, totp.step);
  totp.step = step;
  await stepUp.getByLabel('验证码').fill(code);
  await stepUp.getByRole('button', { name: '验证' }).click();
  await done();
}
