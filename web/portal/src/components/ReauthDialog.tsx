// SPDX-License-Identifier: AGPL-3.0-or-later
// 重新验证框（AUTH-23、UI-09）。受保护操作返回 401 mfa_required 时由请求层打开；
// 用户用密码、TOTP 验证码或恢复码之一完成 POST /v1/me/reauthentications 后，请求层自动重试原操作。
import { zodResolver } from '@hookform/resolvers/zod';
import { useEffect, useId, useState } from 'react';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { isProblemError, unwrap, type ClientApi, type ClientSchemas, type Problem } from '@panel/sdk';
import { Button, Modal, ProblemAlert, TextField, applyFieldErrors } from '@panel/ui';
import { totpCode } from '../forms';

type Method = 'password' | 'totp' | 'recovery_code';

export interface ReauthRequest {
  problem: Problem;
  resolve: (ok: boolean) => void;
}

const schemas = {
  password: z.object({ secret: z.string().min(1, { error: 'validation.required' }).max(128) }),
  totp: z.object({ secret: totpCode }),
  recovery_code: z.object({ secret: z.string().trim().min(1, { error: 'validation.required' }).max(32) }),
} satisfies Record<Method, z.ZodObject<{ secret: z.ZodString }>>;

function methodsOf(p: Problem): Method[] {
  const offered = Array.isArray(p.body.methods) ? p.body.methods : [];
  // 密码总是可用，不列入 methods（spec/30）；Passkey 在 M4 支持。
  return ['password', ...(['totp', 'recovery_code'] as const).filter((m) => offered.includes(m))];
}

export function ReauthDialog({ api, request, onClose }: { api: ClientApi; request: ReauthRequest | null; onClose: () => void }) {
  const { t } = useTranslation();
  const close = (ok: boolean) => {
    request?.resolve(ok);
    onClose();
  };
  return (
    <Modal
      open={!!request}
      onOpenChange={(open) => !open && close(false)}
      title={t('reauth.title')}
      description={t('reauth.description')}
    >
      {request && <ReauthForm api={api} problem={request.problem} onDone={() => close(true)} onCancel={() => close(false)} />}
    </Modal>
  );
}

function ReauthForm({ api, problem, onDone, onCancel }: { api: ClientApi; problem: Problem; onDone: () => void; onCancel: () => void }) {
  const { t } = useTranslation();
  const methods = methodsOf(problem);
  const [method, setMethod] = useState<Method>('password');
  const [formProblem, setFormProblem] = useState<Problem | null>(null);
  const groupId = useId();
  const { register, handleSubmit, setError, reset, formState, setFocus } = useForm<{ secret: string }>({
    resolver: zodResolver(schemas[method]),
    defaultValues: { secret: '' },
  });
  useEffect(() => setFocus('secret'), [method, setFocus]);

  const submit = handleSubmit(async ({ secret }) => {
    setFormProblem(null);
    const challenge = typeof problem.body.challenge_id === 'string' ? { challenge_id: problem.body.challenge_id } : {};
    const body: ClientSchemas['Reauthentication'] =
      method === 'password'
        ? { ...challenge, password: secret }
        : method === 'totp'
          ? { ...challenge, totp_code: secret }
          : { ...challenge, recovery_code: secret.trim() };
    try {
      await unwrap(api.POST('/v1/me/reauthentications', { body }));
    } catch (e) {
      if (!isProblemError(e)) throw e;
      setFormProblem(
        applyFieldErrors(e.problem, { password: 'secret', totp_code: 'secret', recovery_code: 'secret' }, setError, t),
      );
      return;
    }
    onDone();
  });

  const label = t(`reauth.method.${method}`);
  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      {methods.length > 1 && (
        <fieldset className="flex flex-col gap-2">
          <legend className="mb-1 text-sm font-medium">{t('reauth.method_label')}</legend>
          {methods.map((m) => (
            <label key={m} className="flex min-h-9 items-center gap-2 text-sm">
              <input
                type="radio"
                name={groupId}
                value={m}
                checked={method === m}
                onChange={() => {
                  setMethod(m);
                  setFormProblem(null);
                  reset({ secret: '' });
                }}
                className="size-4 accent-primary"
              />
              {t(`reauth.method.${m}`)}
            </label>
          ))}
        </fieldset>
      )}
      <TextField
        key={method}
        label={label}
        type={method === 'password' ? 'password' : 'text'}
        autoComplete={method === 'password' ? 'current-password' : method === 'totp' ? 'one-time-code' : 'off'}
        {...(method === 'totp' ? { inputMode: 'numeric' as const, maxLength: 6 } : {})}
        error={formState.errors.secret?.message && t(formState.errors.secret.message)}
        {...register('secret')}
      />
      <ProblemAlert problem={formProblem} />
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        <Button variant="secondary" onClick={onCancel}>
          {t('cancel')}
        </Button>
        <Button type="submit" loading={formState.isSubmitting}>
          {t('reauth.submit')}
        </Button>
      </div>
    </form>
  );
}
