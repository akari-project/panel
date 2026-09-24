// SPDX-License-Identifier: AGPL-3.0-or-later
// 应用布局：已登录页面用 AppShell（顶栏、导航、页脚），登录等页面用 AuthLayout。
// 导航由应用传入，本包不依赖具体路由库。窄屏下导航收进抽屉（Radix Dialog，焦点受限并可 Esc 关闭）。
import { useState, type ReactNode } from 'react';
import { Dialog } from 'radix-ui';
import { useTranslation } from 'react-i18next';
import { Button } from './Button';
import { LanguageMenu, ThemeMenu } from './Preferences';

export interface SiteInfo {
  siteName: string;
  /** 源代码链接（ARC-04、UI-07），由运行时配置注入 */
  sourceUrl: string;
}

export function SkipLink() {
  const { t } = useTranslation('common');
  return (
    <a
      href="#main"
      className="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-50 focus:rounded focus:bg-surface focus:px-3 focus:py-2"
    >
      {t('skip_to_content')}
    </a>
  );
}

export function Footer({ sourceUrl }: { sourceUrl: string }) {
  const { t } = useTranslation('common');
  return (
    <footer className="border-t border-border px-4 py-4 text-sm text-muted">
      {sourceUrl ? (
        <a href={sourceUrl} rel="noopener" className="underline underline-offset-2 hover:text-fg">
          {t('source_code')}
        </a>
      ) : (
        <span>{t('source_code')}</span>
      )}
    </footer>
  );
}

export interface AppShellProps extends SiteInfo {
  /** 导航链接列表（通常是若干 <li>），同时用于侧栏与移动端抽屉 */
  nav: ReactNode;
  /** 顶栏右侧的附加内容，例如当前用户 */
  account?: ReactNode;
  onSignOut?: () => void;
  children: ReactNode;
}

export function AppShell({ siteName, sourceUrl, nav, account, onSignOut, children }: AppShellProps) {
  const { t } = useTranslation('common');
  const [open, setOpen] = useState(false);
  const navList = <ul className="flex flex-col gap-1">{nav}</ul>;
  return (
    <div className="flex min-h-screen flex-col">
      <SkipLink />
      <header className="sticky top-0 z-30 flex h-14 items-center gap-2 border-b border-border bg-bg/95 px-4">
        <Dialog.Root open={open} onOpenChange={setOpen}>
          <Dialog.Trigger asChild>
            <Button variant="ghost" className="md:hidden" aria-label={t('open_menu')}>
              <span aria-hidden>☰</span>
            </Button>
          </Dialog.Trigger>
          <Dialog.Portal>
            <Dialog.Overlay className="fixed inset-0 z-40 bg-black/50" />
            <Dialog.Content
              // 点击（或键盘激活）抽屉中的链接后关闭抽屉。
              onClick={(e) => {
                if ((e.target as HTMLElement).closest('a')) setOpen(false);
              }}
              className="fixed inset-y-0 left-0 z-50 flex w-72 max-w-[85vw] flex-col gap-4 bg-bg p-4 text-fg shadow-xl">
              <div className="flex items-center justify-between">
                <Dialog.Title className="font-semibold">{siteName}</Dialog.Title>
                <Dialog.Close asChild>
                  <Button variant="ghost" aria-label={t('close_menu')}>
                    <span aria-hidden>✕</span>
                  </Button>
                </Dialog.Close>
              </div>
              <Dialog.Description className="sr-only">{t('main_navigation')}</Dialog.Description>
              <nav aria-label={t('main_navigation')}>{navList}</nav>
            </Dialog.Content>
          </Dialog.Portal>
        </Dialog.Root>
        <span className="font-semibold">{siteName}</span>
        <div className="ml-auto flex items-center gap-1">
          {account}
          <LanguageMenu />
          <ThemeMenu />
          {onSignOut && (
            <Button variant="ghost" onClick={onSignOut}>
              {t('sign_out')}
            </Button>
          )}
        </div>
      </header>
      <div className="flex flex-1">
        <nav aria-label={t('main_navigation')} className="hidden w-56 shrink-0 border-r border-border p-3 md:block">
          {navList}
        </nav>
        <main id="main" tabIndex={-1} className="min-w-0 flex-1 p-4 md:p-6">
          {children}
        </main>
      </div>
      <Footer sourceUrl={sourceUrl} />
    </div>
  );
}

export function AuthLayout({ siteName, sourceUrl, children }: SiteInfo & { children: ReactNode }) {
  return (
    <div className="flex min-h-screen flex-col">
      <SkipLink />
      <header className="flex h-14 items-center gap-2 px-4">
        <span className="font-semibold">{siteName}</span>
        <div className="ml-auto flex items-center gap-1">
          <LanguageMenu />
          <ThemeMenu />
        </div>
      </header>
      <main id="main" tabIndex={-1} className="flex flex-1 items-start justify-center px-4 py-8 sm:items-center">
        <div className="w-full max-w-sm rounded-lg border border-border bg-surface p-6 shadow-sm">{children}</div>
      </main>
      <Footer sourceUrl={sourceUrl} />
    </div>
  );
}

/** 导航项的统一样式，供应用的路由链接使用（配合 data-status="active" 或 aria-current）。 */
export const navLinkClass =
  'flex min-h-10 items-center rounded-md px-3 text-sm hover:bg-border/40 aria-[current=page]:bg-primary/10 aria-[current=page]:font-semibold aria-[current=page]:text-primary';
