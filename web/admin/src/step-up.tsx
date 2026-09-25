// SPDX-License-Identifier: AGPL-3.0-or-later
// 敏感操作的重新验证（spec/10 AUTH-19）：用 TOTP 调用 POST /v1/staff/me/step-up 取得 Mfa-Assertion，
// 在其有效期内（5 分钟，绑定当前会话）缓存于内存，供后续敏感操作复用；不写入任何存储。
// 敏感操作仍返回 401 mfa_required 时（断言过期或会话变化），请求层清除缓存、打开验证框，并以新断言重试原请求。
import { zodResolver } from '@hookform/resolvers/zod';
import { useEffect, useState, useSyncExternalStore } from 'react';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { isProblemError, unwrap, type ConsoleApi, type Problem } from '@panel/sdk';
import { Button, Modal, ProblemAlert, TextField, applyFieldErrors } from '@panel/ui';

// 断言到期前留出余量，避免请求在途中过期。
const MARGIN_MS = 15_000;

export interface StepUp {
  /** 有效的断言；没有时打开验证框。用户取消时返回 null。 */
  ensure: () => Promise<string | null>;
  /** 丢弃缓存并打开验证框。 */
  renew: () => Promise<string | null>;
  /** 登出或会话失效时清除缓存。 */
  clear: () => void;
  subscribe: (fn: () => void) => () => void;
  /** 当前验证框的编号；0 表示未打开。每次打开编号不同，验证框据此重置输入。 */
  prompt: () => number;
  /** 验证框完成：token 为 null 表示取消。 */
  complete: (result: { token: string; expiresAt: number } | null) => void;
}

export function createStepUp(now: () => number = Date.now): StepUp {
  let cached: { token: string; expiresAt: number } | null = null;
  let pending: { id: number; promise: Promise<string | null>; resolve: (t: string | null) => void } | null = null;
  let seq = 0;
  const listeners = new Set<() => void>();
  const notify = () => listeners.forEach((fn) => fn());

  const prompt = () => {
    if (!pending) {
      let resolve!: (t: string | null) => void;
      const promise = new Promise<string | null>((r) => (resolve = r));
      pending = { id: ++seq, promise, resolve };
      notify();
    }
    return pending.promise;
  };

  return {
    ensure: () => (cached && cached.expiresAt - MARGIN_MS > now() ? Promise.resolve(cached.token) : prompt()),
    renew: () => {
      cached = null;
      return prompt();
    },
    clear: () => {
      cached = null;
    },
    subscribe: (fn) => {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
    prompt: () => pending?.id ?? 0,
    complete: (result) => {
      if (result) cached = result;
      const p = pending;
      pending = null;
      p?.resolve(result?.token ?? null);
      notify();
    },
  };
}

const schema = z.object({ code: z.string().regex(/^[0-9]{6}$/, { error: 'validation.totp' }) });

export function StepUpDialog({ api, stepUp }: { api: ConsoleApi; stepUp: StepUp }) {
  const { t } = useTranslation();
  const id = useSyncExternalStore(stepUp.subscribe, stepUp.prompt);
  return (
    <Modal
      open={id > 0}
      onOpenChange={(o) => !o && stepUp.complete(null)}
      title={t('step_up.title')}
      description={t('step_up.description')}
    >
      {id > 0 && <StepUpForm key={id} api={api} stepUp={stepUp} />}
    </Modal>
  );
}

function StepUpForm({ api, stepUp }: { api: ConsoleApi; stepUp: StepUp }) {
  const { t } = useTranslation();
  const tc = useTranslation('common').t;
  const [formProblem, setFormProblem] = useState<Problem | null>(null);
  const { register, handleSubmit, setError, formState, setFocus } = useForm<{ code: string }>({
    resolver: zodResolver(schema),
    defaultValues: { code: '' },
  });
  useEffect(() => setFocus('code'), [setFocus]);

  const submit = handleSubmit(async ({ code }) => {
    setFormProblem(null);
    try {
      // 恢复码不能用于 step-up（StepUpRequest）；Passkey 在 M4。
      const r = await unwrap(api.POST('/v1/staff/me/step-up', { body: { totp_code: code } }));
      stepUp.complete({ token: r.mfa_assertion, expiresAt: Date.parse(r.expires_at) });
    } catch (e) {
      if (!isProblemError(e)) throw e;
      setFormProblem(applyFieldErrors(e.problem, { totp_code: 'code' }, setError, tc));
    }
  });

  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <TextField
        label={tc('sign_in.totp_code')}
        inputMode="numeric"
        autoComplete="one-time-code"
        maxLength={6}
        error={formState.errors.code?.message && tc(formState.errors.code.message)}
        {...register('code')}
      />
      <ProblemAlert problem={formProblem} />
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        <Button variant="secondary" onClick={() => stepUp.complete(null)}>
          {tc('cancel')}
        </Button>
        <Button type="submit" loading={formState.isSubmitting}>
          {t('step_up.submit')}
        </Button>
      </div>
    </form>
  );
}
