// SPDX-License-Identifier: AGPL-3.0-or-later
// 非敏感操作的二次确认（UI-03）：说明影响范围，执行失败时在对话框内按 code 显示错误与 request_id。
// 敏感操作（需要原因与 Mfa-Assertion）使用 sensitive.tsx 的 SensitiveConfirm。
import { useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { isProblemError, type Problem } from '@panel/sdk';
import { Button, Modal, ProblemAlert } from '@panel/ui';

export interface ActionConfirmProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  /** 影响说明（UI-03） */
  description: ReactNode;
  confirmLabel: ReactNode;
  danger?: boolean;
  /** 执行操作；抛出的 ProblemError 显示在对话框中。成功后关闭。 */
  onConfirm: () => Promise<void>;
  /** 按错误给出更具体的文案；返回 undefined 时按 code 显示通用文案。 */
  problemMessage?: (p: Problem) => string | undefined;
}

export function ActionConfirm({ open, onOpenChange, title, description, ...rest }: ActionConfirmProps) {
  return (
    <Modal open={open} onOpenChange={onOpenChange} title={title} description={description}>
      {open && <ConfirmBody {...rest} onClose={() => onOpenChange(false)} />}
    </Modal>
  );
}

function ConfirmBody({
  confirmLabel,
  danger,
  onConfirm,
  problemMessage,
  onClose,
}: Omit<ActionConfirmProps, 'open' | 'onOpenChange' | 'title' | 'description'> & { onClose: () => void }) {
  const tc = useTranslation('common').t;
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<Problem | null>(null);
  const confirm = async () => {
    setBusy(true);
    setProblem(null);
    try {
      await onConfirm();
      onClose();
    } catch (e) {
      if (!isProblemError(e)) throw e;
      setProblem(e.problem);
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <ProblemAlert problem={problem} message={problem ? problemMessage?.(problem) : undefined} />
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        <Button variant="secondary" onClick={onClose} disabled={busy}>
          {tc('cancel')}
        </Button>
        <Button variant={danger ? 'danger' : 'primary'} loading={busy} onClick={() => void confirm()}>
          {confirmLabel}
        </Button>
      </div>
    </>
  );
}

export interface AskOptions {
  title: ReactNode;
  description: ReactNode;
  confirmLabel: ReactNode;
  danger?: boolean;
}

/**
 * 以 Promise 形式询问确认：`await ask({...})` 在确认时为 true，取消或关闭时为 false。
 * 用于表单提交中途的影响确认（UI-03），之后的请求错误仍显示在原表单上。返回的 element 需要渲染。
 */
export function useAsk(): [ReactNode, (o: AskOptions) => Promise<boolean>] {
  const tc = useTranslation('common').t;
  const [state, setState] = useState<(AskOptions & { resolve: (ok: boolean) => void }) | null>(null);
  const close = (ok: boolean) => {
    state?.resolve(ok);
    setState(null);
  };
  const element = (
    <Modal open={!!state} onOpenChange={(o) => !o && close(false)} title={state?.title} description={state?.description}>
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        <Button variant="secondary" onClick={() => close(false)}>
          {tc('cancel')}
        </Button>
        <Button variant={state?.danger ? 'danger' : 'primary'} onClick={() => close(true)}>
          {state?.confirmLabel}
        </Button>
      </div>
    </Modal>
  );
  return [element, (o) => new Promise<boolean>((resolve) => setState({ ...o, resolve }))];
}
