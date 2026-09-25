// SPDX-License-Identifier: AGPL-3.0-or-later
// 恢复码展示，用户中心启用 TOTP 与管理后台首次登录绑定 TOTP 共用（spec/10 AUTH-11、AUTH-21）。
import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from './Button';

export interface RecoveryCodesProps {
  codes: string[];
  onDone: () => void;
  /** 标题层级，随所在页面的结构而定。 */
  headingLevel?: 'h2' | 'h3';
}

/** 只显示一次的恢复码（AUTH-11）：复制、下载，确认已保存后继续。 */
export function RecoveryCodes({ codes, onDone, headingLevel: Heading = 'h2' }: RecoveryCodesProps) {
  const { t } = useTranslation('common');
  const [copied, setCopied] = useState(false);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const text = codes.join('\n') + '\n';
  // 进入此视图时把焦点移到标题，屏幕阅读器读出“请保存恢复码”。
  useEffect(() => headingRef.current?.focus(), []);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  };
  const download = () => {
    const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }));
    const a = document.createElement('a');
    a.href = url;
    a.download = 'recovery-codes.txt';
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 0);
  };

  return (
    <div className="flex flex-col gap-4">
      <Heading ref={headingRef} tabIndex={-1} className="font-semibold">
        {t('recovery_codes.title')}
      </Heading>
      <p className="text-sm">{t('recovery_codes.warning')}</p>
      <ol aria-label={t('recovery_codes.list_label')} className="grid grid-cols-2 gap-2 font-mono text-base select-all">
        {codes.map((c) => (
          <li key={c} className="rounded border border-border bg-bg px-2 py-1 text-center">
            {c}
          </li>
        ))}
      </ol>
      <div className="flex flex-col gap-2 sm:flex-row">
        <Button variant="secondary" onClick={() => void copy()}>
          {t('copy')}
        </Button>
        <Button variant="secondary" onClick={download}>
          {t('download')}
        </Button>
        <Button onClick={onDone}>{t('recovery_codes.done')}</Button>
      </div>
      <p aria-live="polite" className="text-sm text-muted">
        {copied ? t('copied') : ''}
      </p>
    </div>
  );
}
