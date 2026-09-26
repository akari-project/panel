// SPDX-License-Identifier: AGPL-3.0-or-later
// 表单级错误提示：按 code 显示本地化文案与 request_id（UI-02）。
// applyFieldErrors 把 problem 的 errors[] 关联到表单字段（UI-05），返回未能关联的 problem 供表单级提示。
import type { FieldValues, Path, UseFormSetError } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import type { Problem } from '@panel/sdk';
import { useProblemMessage } from './States';

export function ProblemAlert({ problem, message }: { problem: Problem | null; message?: string }) {
  const { t } = useTranslation('common');
  const byCode = useProblemMessage();
  if (!problem) return null;
  return (
    <div role="alert" className="rounded-md border border-danger/50 p-3 text-sm">
      <p className="text-danger">{message ?? byCode(problem)}</p>
      {problem.requestId && <p className="mt-1 font-mono text-muted">{t('request_id', { id: problem.requestId })}</p>}
    </div>
  );
}

/**
 * @param fields 接口字段名到表单字段名的映射
 * @param messageFor 按“字段 + code”给出更具体的文案；返回 undefined 时使用 field_errors 中的通用文案
 */
export function applyFieldErrors<T extends FieldValues>(
  p: Problem,
  fields: Record<string, Path<T>>,
  setError: UseFormSetError<T>,
  t: (key: string) => string,
  messageFor?: (field: string, code: string) => string | undefined,
): Problem | null {
  let mapped = false;
  for (const e of p.errors) {
    const field = fields[e.field];
    if (field) {
      setError(field, { message: messageFor?.(e.field, e.code) ?? t(`field_errors.${e.code}`) }, { shouldFocus: !mapped });
      mapped = true;
    }
  }
  return mapped ? null : p;
}
