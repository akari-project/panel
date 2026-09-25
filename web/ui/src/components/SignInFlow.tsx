// SPDX-License-Identifier: AGPL-3.0-or-later
// 两步登录（spec/10 AUTH-20、AUTH-21）的共用表单。
// 第一步提交邮箱与密码；接口返回 401 mfa_required 时，从 problem 中取 challenge_id、methods
// （管理后台首次登录另有 totp_enrollment），进入第二步提交验证码或恢复码。
// 首次登录同时完成 TOTP 绑定时，接口返回恢复码，先展示恢复码，确认保存后才算登录完成（AUTH-21）。
// 具体的接口调用由应用传入：两份 OpenAPI 的登录接口路径相同、请求体不同。
import { zodResolver } from '@hookform/resolvers/zod';
import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useForm, type FieldValues, type Path, type UseFormSetError } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { isProblemError, type Problem } from '@panel/sdk';
import { Button } from './Button';
import { TextField } from './Field';
import { QrCode } from './QrCode';
import { RecoveryCodes } from './RecoveryCodes';
import { useProblemMessage } from './States';

export type MfaAnswer = { method: 'totp'; code: string } | { method: 'recovery_code'; code: string };

export interface SignInFlowProps {
  title: ReactNode;
  onPassword: (email: string, password: string) => Promise<void>;
  /** 第二步。返回恢复码（首次绑定 TOTP 时）则先展示恢复码。 */
  onMfa: (challengeId: string, answer: MfaAnswer) => Promise<void | { recoveryCodes?: string[] }>;
  onSignedIn: () => void;
}

interface Challenge {
  id: string;
  methods: string[];
  enrollmentSecret?: string;
  enrollmentUri?: string;
}

function challengeOf(p: Problem): Challenge | null {
  if (p.code !== 'mfa_required' || typeof p.body.challenge_id !== 'string') return null;
  const methods = Array.isArray(p.body.methods) ? p.body.methods.filter((m): m is string => typeof m === 'string') : [];
  const enrollment = p.body.totp_enrollment as { secret?: unknown; otpauth_uri?: unknown } | undefined;
  return {
    id: p.body.challenge_id,
    methods,
    ...(typeof enrollment?.secret === 'string' ? { enrollmentSecret: enrollment.secret } : {}),
    ...(typeof enrollment?.otpauth_uri === 'string' ? { enrollmentUri: enrollment.otpauth_uri } : {}),
  };
}

const passwordSchema = z.object({
  email: z.email({ error: 'validation.email' }),
  password: z.string().min(1, { error: 'validation.required' }).max(128),
});
type PasswordValues = z.infer<typeof passwordSchema>;

const totpSchema = z.object({ code: z.string().regex(/^[0-9]{6}$/, { error: 'validation.totp' }) });
const recoverySchema = z.object({ code: z.string().trim().min(1, { error: 'validation.required' }).max(32) });
type CodeValues = { code: string };

/** 把 problem 的 errors[] 关联到表单字段；返回未能关联的 problem 供表单级提示。 */
function applyProblem<T extends FieldValues>(p: Problem, fields: readonly string[], setError: UseFormSetError<T>, t: (k: string) => string) {
  let mapped = false;
  for (const e of p.errors) {
    if (fields.includes(e.field)) {
      setError(e.field as Path<T>, { message: t(`field_errors.${e.code}`) });
      mapped = true;
    }
  }
  return mapped ? null : p;
}

function FormError({ problem }: { problem: Problem | null }) {
  const { t } = useTranslation('common');
  const message = useProblemMessage();
  if (!problem) return null;
  return (
    <div role="alert" className="rounded-md border border-danger/50 p-3 text-sm">
      {/* 登录接口的 unauthenticated 表示凭据不正确，而不是登录失效。 */}
      <p className="text-danger">{problem.code === 'unauthenticated' ? t('sign_in.failed') : message(problem)}</p>
      {problem.requestId && <p className="mt-1 font-mono text-muted">{t('request_id', { id: problem.requestId })}</p>}
    </div>
  );
}

function PasswordStep({ onSubmit }: { onSubmit: (v: PasswordValues) => Promise<Problem | null> }) {
  const { t } = useTranslation('common');
  const [formProblem, setFormProblem] = useState<Problem | null>(null);
  const { register, handleSubmit, setError, formState } = useForm<PasswordValues>({ resolver: zodResolver(passwordSchema) });
  const submit = handleSubmit(async (v) => {
    setFormProblem(null);
    const p = await onSubmit(v);
    if (p) setFormProblem(applyProblem(p, ['email', 'password'], setError, t));
  });
  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <TextField
        label={t('sign_in.email')}
        type="email"
        autoComplete="username"
        // 登录页的唯一任务是输入邮箱，自动聚焦符合预期。
        // eslint-disable-next-line jsx-a11y/no-autofocus
        autoFocus
        error={formState.errors.email?.message && t(formState.errors.email.message)}
        {...register('email')}
      />
      <TextField
        label={t('sign_in.password')}
        type="password"
        autoComplete="current-password"
        error={formState.errors.password?.message && t(formState.errors.password.message)}
        {...register('password')}
      />
      <FormError problem={formProblem} />
      <Button type="submit" loading={formState.isSubmitting}>
        {t('sign_in.submit')}
      </Button>
    </form>
  );
}

function MfaStep({
  challenge,
  onSubmit,
  onBack,
}: {
  challenge: Challenge;
  onSubmit: (a: MfaAnswer) => Promise<Problem | null>;
  onBack: () => void;
}) {
  const { t } = useTranslation('common');
  const canRecover = challenge.methods.includes('recovery_code') && !challenge.enrollmentSecret;
  const [method, setMethod] = useState<'totp' | 'recovery_code'>('totp');
  const [formProblem, setFormProblem] = useState<Problem | null>(null);
  const { register, handleSubmit, setError, reset, formState, setFocus } = useForm<CodeValues>({
    resolver: zodResolver(method === 'totp' ? totpSchema : recoverySchema),
  });
  useEffect(() => setFocus('code'), [method, setFocus]);
  const submit = handleSubmit(async (v) => {
    setFormProblem(null);
    const p = await onSubmit({ method, code: v.code });
    if (p) setFormProblem(applyProblem(p, ['totp_code', 'recovery_code'], (_f, e) => setError('code', e), t));
  });
  const switchMethod = () => {
    setMethod((m) => (m === 'totp' ? 'recovery_code' : 'totp'));
    setFormProblem(null);
    reset();
  };
  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <h2 className="text-lg font-semibold">{t('sign_in.mfa_title')}</h2>
      {challenge.enrollmentSecret ? (
        <div className="flex flex-col gap-2 text-sm">
          <p>{t('sign_in.enroll_hint')}</p>
          {challenge.enrollmentUri && <QrCode value={challenge.enrollmentUri} label={t('sign_in.enroll_qr')} />}
          <p>
            {t('sign_in.enroll_secret')}: <code className="font-mono break-all select-all">{challenge.enrollmentSecret}</code>
          </p>
        </div>
      ) : (
        method === 'totp' && <p className="text-sm text-muted">{t('sign_in.mfa_hint')}</p>
      )}
      {method === 'totp' ? (
        <TextField
          key="totp"
          label={t('sign_in.totp_code')}
          inputMode="numeric"
          autoComplete="one-time-code"
          maxLength={6}
          error={formState.errors.code?.message && t(formState.errors.code.message)}
          {...register('code')}
        />
      ) : (
        <TextField
          key="recovery"
          label={t('sign_in.recovery_code')}
          autoComplete="off"
          error={formState.errors.code?.message && t(formState.errors.code.message)}
          {...register('code')}
        />
      )}
      <FormError problem={formProblem} />
      <Button type="submit" loading={formState.isSubmitting}>
        {t('sign_in.verify')}
      </Button>
      <div className="flex justify-between gap-2">
        <Button variant="ghost" onClick={onBack}>
          {t('sign_in.back')}
        </Button>
        {canRecover && (
          <Button variant="ghost" onClick={switchMethod}>
            {method === 'totp' ? t('sign_in.use_recovery') : t('sign_in.use_totp')}
          </Button>
        )}
      </div>
    </form>
  );
}

export function SignInFlow({ title, onPassword, onMfa, onSignedIn }: SignInFlowProps) {
  const [challenge, setChallenge] = useState<Challenge | null>(null);
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null);
  const heading = useRef<HTMLHeadingElement>(null);

  // 返回 null 表示已处理（成功或进入下一步）；返回 Problem 由表单显示。
  const run = async (fn: () => Promise<void | { recoveryCodes?: string[] }>): Promise<Problem | null> => {
    try {
      const r = await fn();
      if (r?.recoveryCodes?.length) setRecoveryCodes(r.recoveryCodes);
      else onSignedIn();
      return null;
    } catch (e) {
      if (!isProblemError(e)) throw e;
      const next = challengeOf(e.problem);
      if (next) {
        setChallenge(next);
        return null;
      }
      return e.problem;
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <h1 ref={heading} tabIndex={-1} className="text-xl font-semibold">
        {title}
      </h1>
      {recoveryCodes ? (
        <RecoveryCodes codes={recoveryCodes} onDone={onSignedIn} />
      ) : challenge ? (
        <MfaStep
          challenge={challenge}
          onSubmit={(a) => run(() => onMfa(challenge.id, a))}
          onBack={() => {
            setChallenge(null);
            heading.current?.focus();
          }}
        />
      ) : (
        <PasswordStep onSubmit={(v) => run(() => onPassword(v.email, v.password))} />
      )}
    </div>
  );
}
