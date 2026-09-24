// SPDX-License-Identifier: AGPL-3.0-or-later
// 主题与语言切换菜单。使用 Radix DropdownMenu，键盘可操作（方向键、Enter、Esc）。
import { DropdownMenu } from 'radix-ui';
import { useTranslation } from 'react-i18next';
import { supportedLanguages } from '../i18n';
import { useTheme, type ThemePreference } from '../theme';
import { Button } from './Button';

const itemClass =
  'flex min-h-9 cursor-default select-none items-center gap-2 rounded px-3 text-sm outline-none data-[highlighted]:bg-border/50';
const contentClass = 'z-50 min-w-40 rounded-md border border-border bg-surface p-1 text-fg shadow-lg';

function Check() {
  return (
    <DropdownMenu.ItemIndicator>
      <span aria-hidden>✓</span>
    </DropdownMenu.ItemIndicator>
  );
}

export function ThemeMenu() {
  const { t } = useTranslation('common');
  const { preference, setPreference } = useTheme();
  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <Button variant="ghost" aria-label={t('theme.label')}>
          <span aria-hidden>{preference === 'dark' ? '☾' : preference === 'light' ? '☀' : '◐'}</span>
          <span className="hidden sm:inline">{t(`theme.${preference}`)}</span>
        </Button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content align="end" sideOffset={4} className={contentClass}>
          <DropdownMenu.RadioGroup value={preference} onValueChange={(v) => setPreference(v as ThemePreference)}>
            {(['light', 'dark', 'system'] as const).map((p) => (
              <DropdownMenu.RadioItem key={p} value={p} className={itemClass}>
                <span className="w-4">
                  <Check />
                </span>
                {t(`theme.${p}`)}
              </DropdownMenu.RadioItem>
            ))}
          </DropdownMenu.RadioGroup>
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  );
}

export function LanguageMenu() {
  const { t, i18n } = useTranslation('common');
  const current = i18n.resolvedLanguage ?? i18n.language;
  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <Button variant="ghost" aria-label={t('language.label')}>
          <span aria-hidden>文A</span>
          <span className="hidden sm:inline">{t(`language.${current}`, { defaultValue: current })}</span>
        </Button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content align="end" sideOffset={4} className={contentClass}>
          <DropdownMenu.RadioGroup value={current} onValueChange={(v) => void i18n.changeLanguage(v)}>
            {supportedLanguages.map((l) => (
              <DropdownMenu.RadioItem key={l} value={l} lang={l} className={itemClass}>
                <span className="w-4">
                  <Check />
                </span>
                {t(`language.${l}`)}
              </DropdownMenu.RadioItem>
            ))}
          </DropdownMenu.RadioGroup>
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  );
}
