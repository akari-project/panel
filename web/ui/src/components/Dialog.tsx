// SPDX-License-Identifier: AGPL-3.0-or-later
// 模态对话框（Radix Dialog）：焦点受限、Esc 关闭、关闭后焦点回到触发元素。
// ConfirmDialog 用于危险操作的二次确认，并说明影响范围（UI-03）。
import { Dialog as D } from 'radix-ui';
import { useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from './Button';

export interface ModalProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  description?: ReactNode;
  children?: ReactNode;
}

export function Modal({ open, onOpenChange, title, description, children }: ModalProps) {
  return (
    <D.Root open={open} onOpenChange={onOpenChange}>
      <D.Portal>
        <D.Overlay className="fixed inset-0 z-40 bg-black/50" />
        <D.Content
          // 没有描述时显式声明，避免 Radix 的无障碍警告。
          {...(description ? {} : { 'aria-describedby': undefined })}
          className="fixed top-1/2 left-1/2 z-50 flex max-h-[90vh] w-[calc(100vw-2rem)] max-w-md -translate-x-1/2 -translate-y-1/2 flex-col gap-4 overflow-y-auto rounded-lg border border-border bg-bg p-6 text-fg shadow-xl"
        >
          <D.Title className="text-lg font-semibold">{title}</D.Title>
          {description && <D.Description className="text-sm text-muted">{description}</D.Description>}
          {children}
        </D.Content>
      </D.Portal>
    </D.Root>
  );
}

export interface ConfirmDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  /** 影响范围说明（UI-03） */
  description: ReactNode;
  confirmLabel: ReactNode;
  onConfirm: () => Promise<void> | void;
  danger?: boolean;
}

export function ConfirmDialog({ open, onOpenChange, title, description, confirmLabel, onConfirm, danger }: ConfirmDialogProps) {
  const { t } = useTranslation('common');
  const [busy, setBusy] = useState(false);
  const confirm = async () => {
    setBusy(true);
    try {
      await onConfirm();
    } finally {
      setBusy(false);
    }
  };
  return (
    <Modal open={open} onOpenChange={(o) => !busy && onOpenChange(o)} title={title} description={description}>
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        <Button variant="secondary" onClick={() => onOpenChange(false)} disabled={busy}>
          {t('cancel')}
        </Button>
        <Button
          variant={danger ? 'danger' : 'primary'}
          loading={busy}
          onClick={() => void confirm()}
        >
          {confirmLabel}
        </Button>
      </div>
    </Modal>
  );
}
