// SPDX-License-Identifier: AGPL-3.0-or-later
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { I18nextProvider } from 'react-i18next';
import type { ReactNode } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { ProblemError, toProblem } from '@panel/sdk';
import { createI18n } from '../i18n';
import { ThemeProvider } from '../theme';
import { TextField } from './Field';
import { ErrorState } from './States';
import { AppShell } from './Layout';
import { ConfirmDialog } from './Dialog';
import { QrCode } from './QrCode';

function wrap(children: ReactNode, lng: 'zh-CN' | 'en' = 'zh-CN') {
  const i18n = createI18n({ resources: { 'zh-CN': {}, en: {} }, defaultNS: 'common', lng });
  return (
    <I18nextProvider i18n={i18n}>
      <ThemeProvider>{children}</ThemeProvider>
    </I18nextProvider>
  );
}

describe('TextField', () => {
  it('错误文案与字段关联', () => {
    render(<TextField label="邮箱" error="必填" />);
    const input = screen.getByLabelText('邮箱');
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(input).toHaveAccessibleDescription('必填');
  });
});

describe('ErrorState', () => {
  it('按 code 显示本地化文案与 request_id', () => {
    const err = new ProblemError(toProblem({ code: 'rate_limited', request_id: 'req_42' }, new Response(null, { status: 429 })));
    render(wrap(<ErrorState error={err} />));
    expect(screen.getByRole('alert')).toHaveTextContent('操作过于频繁');
    expect(screen.getByText('请求编号：req_42')).toBeInTheDocument();
  });

  it('英文文案', () => {
    const err = new ProblemError(toProblem({ code: 'forbidden' }, new Response(null, { status: 403 })));
    render(wrap(<ErrorState error={err} />, 'en'));
    expect(screen.getByRole('alert')).toHaveTextContent("You don't have permission");
  });
});

describe('AppShell', () => {
  it('页脚显示源代码链接，主题切换改变 html 的 dark 类', async () => {
    render(
      wrap(
        <AppShell siteName="Akari" sourceUrl="https://example.invalid/src" nav={<li>nav</li>}>
          <p>content</p>
        </AppShell>,
      ),
    );
    expect(screen.getByRole('link', { name: '源代码' })).toHaveAttribute('href', 'https://example.invalid/src');
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: '主题' }));
    await user.click(await screen.findByRole('menuitemradio', { name: /深色/ }));
    expect(document.documentElement).toHaveClass('dark');
    expect(localStorage.getItem('panel.theme')).toBe('dark');
  });
});

describe('QrCode', () => {
  it('以带名称的 SVG 绘制，不使用内联样式', () => {
    render(<QrCode value="otpauth://totp/Akari:alice?secret=JBSWY3DPEHPK3PXP&issuer=Akari" label="二维码" />);
    const img = screen.getByRole('img', { name: '二维码' });
    expect(img.tagName.toLowerCase()).toBe('svg');
    expect(img.querySelector('path')?.getAttribute('d')).toMatch(/^M\d/);
    expect(img.outerHTML).not.toContain('style=');
  });
});

describe('ConfirmDialog', () => {
  it('显示影响范围，确认后调用 onConfirm，取消时关闭', async () => {
    const onConfirm = vi.fn();
    const onOpenChange = vi.fn();
    render(
      wrap(
        <ConfirmDialog open onOpenChange={onOpenChange} title="停用？" description="将影响 3 台设备" confirmLabel="停用" onConfirm={onConfirm} danger />,
      ),
    );
    const dialog = screen.getByRole('dialog', { name: '停用？' });
    expect(dialog).toHaveAccessibleDescription('将影响 3 台设备');
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: '停用' }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole('button', { name: '取消' }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});
