// SPDX-License-Identifier: AGPL-3.0-or-later
// 敏感操作（spec/10 AUTH-19、spec/31 CON-03）的界面部分：
// - 原因必填（最长 500 字符）：请求体的 reason，DELETE 改用请求头 Audit-Reason（UTF-8 百分号编码）；
// - 执行前取得 Mfa-Assertion（见 step-up.tsx）；
// - 界面二次确认并说明影响（UI-03）。
// 产生副作用的 POST 在一次确认内使用同一个 Idempotency-Key，请求层重试时原样携带（CONV-12）。
import { zodResolver } from '@hookform/resolvers/zod';
import { useRouteContext } from '@tanstack/react-router';
import { forwardRef, useRef, useState, type ReactNode } from 'react';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { isProblemError, type Problem } from '@panel/sdk';
import { Button, Modal, ProblemAlert, TextAreaField, applyFieldErrors, type TextAreaFieldProps } from '@panel/ui';

export const REASON_MAX = 500;

export const reasonSchema = z
  .string()
  .trim()
  .min(1, { error: 'validation.required' })
  .max(REASON_MAX, { error: 'field_errors.too_long' });

export function newIdempotencyKey(): string {
  return crypto.randomUUID();
}

/**
 * 按请求体取幂等键：请求体与上次相同时沿用上次的键（重复提交与重试得到同一结果），
 * 不同时换新键——服务端缓存 4xx 响应，改了内容还用旧键会得到 422 idempotency_key_reused（CONV-12）。
 */
export function useIdempotencyKey() {
  const last = useRef<{ body: string; key: string } | null>(null);
  return (body: unknown): string => {
    const text = JSON.stringify(body);
    if (last.current?.body !== text) last.current = { body: text, key: newIdempotencyKey() };
    return last.current.key;
  };
}

/** DELETE 请求的原因请求头（CON-03）。 */
export function auditReasonHeader(reason: string): string {
  return encodeURIComponent(reason.trim());
}

/** 用户取消重新验证时抛出，调用方静默处理。 */
export class StepUpCancelled extends Error {
  constructor() {
    super('step-up cancelled');
    this.name = 'StepUpCancelled';
  }
}

/** 取得 Mfa-Assertion 后执行 fn；用户取消验证时抛出 StepUpCancelled。 */
export function useSensitive() {
  const { stepUp } = useRouteContext({ from: '__root__' });
  return async <T,>(fn: (assertion: string) => Promise<T>): Promise<T> => {
    const assertion = await stepUp.ensure();
    if (!assertion) throw new StepUpCancelled();
    return fn(assertion);
  };
}

export const ReasonField = forwardRef<HTMLTextAreaElement, Omit<TextAreaFieldProps, 'label'>>(function ReasonField(props, ref) {
  const { t } = useTranslation();
  return <TextAreaField ref={ref} label={t('sensitive.reason')} hint={t('sensitive.reason_hint')} maxLength={REASON_MAX} {...props} />;
});

/**
 * 处理敏感操作的错误：取消验证时返回 null（不提示）；
 * 其余 ProblemError 先关联到表单字段，未关联的返回供表单级提示。
 */
export function handleSensitiveError(
  e: unknown,
  apply: (p: Problem) => Problem | null,
): Problem | null {
  if (e instanceof StepUpCancelled) return null;
  if (!isProblemError(e)) throw e;
  return apply(e.problem);
}

export interface SensitiveConfirmProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  /** 影响说明（UI-03） */
  description: ReactNode;
  confirmLabel: ReactNode;
  /** 执行操作；抛出的 ProblemError 显示在对话框中。成功后关闭。 */
  onConfirm: (reason: string) => Promise<void>;
}

/** 没有其他输入项的敏感操作（移除、撤销、删除）：二次确认 + 原因。 */
export function SensitiveConfirm({ open, onOpenChange, title, description, confirmLabel, onConfirm }: SensitiveConfirmProps) {
  return (
    <Modal open={open} onOpenChange={onOpenChange} title={title} description={description}>
      {open && <ConfirmForm confirmLabel={confirmLabel} onConfirm={onConfirm} onClose={() => onOpenChange(false)} />}
    </Modal>
  );
}

function ConfirmForm({
  confirmLabel,
  onConfirm,
  onClose,
}: {
  confirmLabel: ReactNode;
  onConfirm: (reason: string) => Promise<void>;
  onClose: () => void;
}) {
  const tc = useTranslation('common').t;
  const [problem, setProblem] = useState<Problem | null>(null);
  const { register, handleSubmit, setError, formState } = useForm<{ reason: string }>({
    resolver: zodResolver(z.object({ reason: reasonSchema })),
    defaultValues: { reason: '' },
  });
  const submit = handleSubmit(async ({ reason }) => {
    setProblem(null);
    try {
      await onConfirm(reason);
      onClose();
    } catch (e) {
      setProblem(handleSensitiveError(e, (p) => applyFieldErrors(p, { reason: 'reason' }, setError, tc)));
    }
  });
  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <ReasonField
        error={formState.errors.reason?.message && tc(formState.errors.reason.message)}
        {...register('reason')}
      />
      <ProblemAlert problem={problem} />
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        <Button variant="secondary" onClick={onClose} disabled={formState.isSubmitting}>
          {tc('cancel')}
        </Button>
        <Button type="submit" className="bg-danger text-white hover:bg-danger/90 dark:text-bg" loading={formState.isSubmitting}>
          {confirmLabel}
        </Button>
      </div>
    </form>
  );
}
