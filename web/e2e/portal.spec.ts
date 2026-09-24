// SPDX-License-Identifier: AGPL-3.0-or-later
// M0-06 验收 2、4：用户中心在 Mock 下登录并进入空白首页；产物以相对路径加载、读取注入的运行时配置。
import { PORTAL, expect, test } from './fixtures';

test('未登录跳转到登录页，登录后进入首页', async ({ page }) => {
  await page.goto(`${PORTAL}/`);
  await expect(page).toHaveURL(/\/login\?redirect=%2F$/);
  await expect(page.getByRole('heading', { name: '登录' })).toBeVisible();
  // 站点名称与源代码链接来自注入的 window.__PANEL_CONFIG__（UI-06、UI-07）。
  await expect(page).toHaveTitle('Akari');
  await expect(page.getByRole('link', { name: '源代码' })).toHaveAttribute('href', 'https://github.com/akari-project/panel/tree/mock-revision');

  await page.getByLabel('邮箱').fill('alice@example.com');
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '登录' }).click();

  await expect(page).toHaveURL(`${PORTAL}/`);
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();
  await expect(page.getByText('欢迎，alice@example.com')).toBeVisible();

  await page.getByRole('button', { name: '退出登录' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await page.goto(`${PORTAL}/`);
  await expect(page).toHaveURL(/\/login/);
});

test('表单校验错误与字段关联', async ({ page }) => {
  await page.goto(`${PORTAL}/login`);
  await page.getByRole('button', { name: '登录' }).click();
  const email = page.getByLabel('邮箱');
  await expect(email).toHaveAttribute('aria-invalid', 'true');
  await expect(email).toHaveAccessibleDescription('请输入有效的邮箱地址');
});

test('账号启用二次验证时进入第二步', async ({ page }) => {
  await page.goto(`${PORTAL}/login`);
  await page.getByLabel('邮箱').fill('mfa@example.com');
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '登录' }).click();
  await expect(page.getByRole('heading', { name: '二次验证' })).toBeVisible();
  await page.getByLabel('验证码').fill('731946');
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();
});

test('只用键盘完成登录', async ({ page }) => {
  await page.goto(`${PORTAL}/login`);
  await expect(page.getByLabel('邮箱')).toBeFocused();
  await page.keyboard.type('alice@example.com');
  await page.keyboard.press('Tab');
  await page.keyboard.type('correct-horse-battery');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();
});

test('深浅色主题切换并在刷新后保持', async ({ page }) => {
  await page.goto(`${PORTAL}/login`);
  await page.getByRole('button', { name: '主题' }).click();
  await page.getByRole('menuitemradio', { name: '深色' }).click();
  await expect(page.locator('html')).toHaveClass(/dark/);
  await page.reload();
  await expect(page.locator('html')).toHaveClass(/dark/);
  await page.getByRole('button', { name: '主题' }).click();
  await page.getByRole('menuitemradio', { name: '浅色' }).click();
  await expect(page.locator('html')).not.toHaveClass(/dark/);
});

test('切换语言为英文', async ({ page }) => {
  await page.goto(`${PORTAL}/login`);
  await page.getByRole('button', { name: '语言' }).click();
  await page.getByRole('menuitemradio', { name: 'English' }).click();
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  await expect(page.locator('html')).toHaveAttribute('lang', 'en');
});

test('深层路径的回退页面仍能加载资源', async ({ page }) => {
  // index.html 中的相对路径由服务端按 base_path 重写，深层路径下资源仍能加载，路由显示 404 页。
  await page.goto(`${PORTAL}/a/b/c`);
  await expect(page.getByRole('heading', { name: '页面不存在' })).toBeVisible();
  await page.getByRole('link', { name: '返回首页' }).click();
  await expect(page).toHaveURL(/\/login\?redirect=%2F$/);
});

test('移动端：抽屉导航 @mobile', async ({ page }) => {
  await page.goto(`${PORTAL}/login`);
  await page.getByLabel('邮箱').fill('alice@example.com');
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '登录' }).click();
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();
  await page.getByRole('button', { name: '打开菜单' }).click();
  const drawer = page.getByRole('dialog');
  await expect(drawer.getByRole('link', { name: '概览' })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(drawer).toBeHidden();
});
