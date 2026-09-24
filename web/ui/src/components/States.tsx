// SPDX-License-Identifier: AGPL-3.0-or-later
// 数据视图的加载、空、错误三种状态（UI-02）。错误按 code 显示本地化文案，并显示 request_id。
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { isProblemError, toProblem, type Problem } from '@panel/sdk';
import { Button } from './Button';

export function LoadingState({ label }: { label?: string }) {
  const { t } = useTranslation('common');
  return (
    <div role="status" className="flex items-center gap-3 p-6 text-muted">
      <span aria-hidden className="size-4 animate-spin rounded-full border-2 border-border border-t-primary" />
      <span>{label ?? t('loading')}</span>
    </div>
  );
}

export function EmptyState({ title, children }: { title?: string; children?: ReactNode }) {
  const { t } = useTranslation('common');
  return (
    <div className="flex flex-col items-center gap-2 rounded-lg border border-dashed border-border p-10 text-center">
      <p className="font-medium">{title ?? t('empty')}</p>
      {children && <div className="text-sm text-muted">{children}</div>}
    </div>
  );
}

export function problemOf(error: unknown): Problem {
  return isProblemError(error) ? error.problem : toProblem(undefined);
}

/** 按 code 取本地化错误文案；未知 code 显示通用文案。 */
export function useProblemMessage() {
  const { t } = useTranslation('common');
  return (p: Problem) => t(`errors.${p.code}`, { defaultValue: t('errors.unknown') });
}

export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const { t } = useTranslation('common');
  const message = useProblemMessage();
  const p = problemOf(error);
  return (
    <div role="alert" className="flex flex-col gap-2 rounded-lg border border-danger/50 p-6">
      <p className="font-medium text-danger">{message(p)}</p>
      {p.requestId && <p className="font-mono text-sm text-muted">{t('request_id', { id: p.requestId })}</p>}
      {onRetry && (
        <div>
          <Button variant="secondary" onClick={onRetry}>
            {t('retry')}
          </Button>
        </div>
      )}
    </div>
  );
}
