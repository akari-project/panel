// SPDX-License-Identifier: AGPL-3.0-or-later
// 账号安全（spec/32）：TOTP 二次验证与恢复码（AUTH-11）、修改密码。
// 停用二次验证、重新生成恢复码、修改密码需要重新验证（AUTH-23），由请求层弹出验证框后自动重试（UI-09）。
import { zodResolver } from '@hookform/resolvers/zod';
import { useQueryClient, useSuspenseQuery } from '@tanstack/react-query';
import { useRouteContext } from '@tanstack/react-router';
import { useId, useState, type ReactNode } from 'react';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { isProblemError, unwrap, type Problem } from '@panel/sdk';
import {
  Button,
  ConfirmDialog,
  ProblemAlert,
  QrCode,
  RecoveryCodes,
  TextField,
  applyFieldErrors,
  formatDateTime,
} from '@panel/ui';
import { newPassword, PASSWORD_MAX, PASSWORD_MIN, totpCode } from '../forms';
import { meQuery } from '../queries';

export function SecurityPage() {
  const { t } = useTranslation();
  return (
    <div className="flex max-w-2xl flex-col gap-8">
      <h1 className="text-2xl font-semibold">{t('security.title')}</h1>
      <TwoFactorSection />
      <PasswordSection />
    </div>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  const id = useId();
  return (
    <section aria-labelledby={id} className="flex flex-col gap-4 rounded-lg border border-border bg-surface p-4 sm:p-6">
      <h2 id={id} className="text-lg font-semibold">
        {title}
      </h2>
      {children}
    </section>
  );
}

/** 用户取消重新验证时，请求返回原来的 mfa_required，不再额外提示。 */
function visibleProblem(e: unknown): Problem | null {
  if (!isProblemError(e)) throw e;
  return e.problem.code === 'mfa_required' ? null : e.problem;
}

type Enrollment = { secret: string; otpauthUri: string; expiresAt: string };
type TwoFactorView = { step: 'status' } | { step: 'enroll'; enrollment: Enrollment } | { step: 'codes'; codes: string[] };

function TwoFactorSection() {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const queryClient = useQueryClient();
  const { data: me } = useSuspenseQuery(meQuery(api));
  const [view, setView] = useState<TwoFactorView>({ step: 'status' });
  const [problem, setProblem] = useState<Problem | null>(null);
  const [busy, setBusy] = useState(false);
  const [confirm, setConfirm] = useState<'disable' | 'regenerate' | null>(null);
  const refreshMe = () => queryClient.invalidateQueries({ queryKey: ['me'] });

  const startEnrollment = async () => {
    setProblem(null);
    setBusy(true);
    try {
      const r = await unwrap(api.POST('/v1/me/mfa/totp'));
      setView({ step: 'enroll', enrollment: { secret: r.secret, otpauthUri: r.otpauth_uri, expiresAt: r.expires_at } });
    } catch (e) {
      const p = visibleProblem(e);
      // 已在其他地方启用（409 invalid_state）：刷新状态。
      if (p?.code === 'invalid_state') await refreshMe();
      setProblem(p);
    } finally {
      setBusy(false);
    }
  };

  const disable = async () => {
    setProblem(null);
    try {
      await unwrap(api.DELETE('/v1/me/mfa/totp'));
      await refreshMe();
    } catch (e) {
      setProblem(visibleProblem(e));
    }
    setConfirm(null);
  };

  const regenerate = async () => {
    setProblem(null);
    try {
      const r = await unwrap(api.POST('/v1/me/mfa/recovery-codes'));
      setView({ step: 'codes', codes: r.recovery_codes });
    } catch (e) {
      setProblem(visibleProblem(e));
    }
    setConfirm(null);
  };

  if (view.step === 'enroll') {
    return (
      <Section title={t('security.mfa.title')}>
        <EnrollTotp
          enrollment={view.enrollment}
          timeZone={me.timezone}
          onActivated={(codes) => {
            void refreshMe();
            setView({ step: 'codes', codes });
          }}
          onCancel={() => setView({ step: 'status' })}
        />
      </Section>
    );
  }
  if (view.step === 'codes') {
    return (
      <Section title={t('security.mfa.title')}>
        <RecoveryCodes headingLevel="h3" codes={view.codes} onDone={() => setView({ step: 'status' })} />
      </Section>
    );
  }
  return (
    <Section title={t('security.mfa.title')}>
      <p className="text-sm text-muted">{t('security.mfa.description')}</p>
      <p>
        <span className="font-medium">{t('security.mfa.status')}</span>{' '}
        {me.is_mfa_enabled ? t('security.mfa.enabled') : t('security.mfa.disabled')}
      </p>
      <ProblemAlert problem={problem} />
      {me.is_mfa_enabled ? (
        <div className="flex flex-col gap-2 sm:flex-row">
          <Button variant="secondary" onClick={() => setConfirm('regenerate')}>
            {t('security.mfa.regenerate')}
          </Button>
          <Button variant="secondary" onClick={() => setConfirm('disable')}>
            {t('security.mfa.disable')}
          </Button>
        </div>
      ) : (
        <div>
          <Button onClick={() => void startEnrollment()} loading={busy}>
            {t('security.mfa.enable')}
          </Button>
        </div>
      )}
      <ConfirmDialog
        open={confirm === 'disable'}
        onOpenChange={(o) => !o && setConfirm(null)}
        title={t('security.mfa.disable_title')}
        description={t('security.mfa.disable_impact')}
        confirmLabel={t('security.mfa.disable')}
        danger
        onConfirm={disable}
      />
      <ConfirmDialog
        open={confirm === 'regenerate'}
        onOpenChange={(o) => !o && setConfirm(null)}
        title={t('security.mfa.regenerate_title')}
        description={t('security.mfa.regenerate_impact')}
        confirmLabel={t('security.mfa.regenerate')}
        onConfirm={regenerate}
      />
    </Section>
  );
}

/** Base32 密钥每 4 个字符一组显示，便于手动输入。 */
function groupSecret(secret: string) {
  return secret.replace(/(.{4})(?=.)/g, '$1 ');
}

function EnrollTotp({
  enrollment,
  timeZone,
  onActivated,
  onCancel,
}: {
  enrollment: Enrollment;
  timeZone: string;
  onActivated: (codes: string[]) => void;
  onCancel: () => void;
}) {
  const { t, i18n } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const [problem, setProblem] = useState<Problem | null>(null);
  const { register, handleSubmit, setError, formState, setFocus } = useForm<{ code: string }>({
    resolver: zodResolver(z.object({ code: totpCode })),
    defaultValues: { code: '' },
  });
  const secretId = useId();

  const submit = handleSubmit(async ({ code }) => {
    setProblem(null);
    try {
      const r = await unwrap(api.POST('/v1/me/mfa/totp/activation', { body: { totp_code: code } }));
      onActivated(r.recovery_codes);
    } catch (e) {
      const p = visibleProblem(e);
      setProblem(p && applyFieldErrors(p, { totp_code: 'code' }, setError, t));
      if (!p || p.errors.length) setFocus('code');
    }
  });

  return (
    <div className="flex flex-col gap-4">
      <ol className="flex list-decimal flex-col gap-1 pl-5 text-sm">
        <li>{t('security.enroll.step_scan')}</li>
        <li>{t('security.enroll.step_code')}</li>
      </ol>
      <div className="self-start">
        <QrCode value={enrollment.otpauthUri} label={t('security.enroll.qr_label')} />
      </div>
      <div className="flex flex-col gap-1">
        <p id={secretId} className="text-sm font-medium">
          {t('security.enroll.secret')}
        </p>
        <code aria-labelledby={secretId} data-secret={enrollment.secret} className="font-mono text-base break-all select-all">
          {groupSecret(enrollment.secret)}
        </code>
        <p className="text-sm text-muted">
          {t('security.enroll.expires', { time: formatDateTime(enrollment.expiresAt, i18n.resolvedLanguage ?? 'en', timeZone || undefined) })}
        </p>
      </div>
      <form noValidate onSubmit={submit} className="flex flex-col gap-4">
        <TextField
          label={t('security.enroll.code')}
          inputMode="numeric"
          autoComplete="one-time-code"
          maxLength={6}
          error={formState.errors.code?.message && t(formState.errors.code.message)}
          {...register('code')}
        />
        <ProblemAlert problem={problem} />
        <div className="flex flex-col gap-2 sm:flex-row">
          <Button type="submit" loading={formState.isSubmitting}>
            {t('security.enroll.submit')}
          </Button>
          <Button variant="ghost" onClick={onCancel}>
            {t('cancel')}
          </Button>
        </div>
      </form>
    </div>
  );
}

const passwordSchema = z
  .object({ password: newPassword, confirm: z.string() })
  .refine((v) => v.password === v.confirm, { path: ['confirm'], error: 'validation.password_mismatch' });
type PasswordValues = z.infer<typeof passwordSchema>;

function PasswordSection() {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const [problem, setProblem] = useState<Problem | null>(null);
  const [done, setDone] = useState(false);
  const { register, handleSubmit, setError, formState, reset } = useForm<PasswordValues>({
    resolver: zodResolver(passwordSchema),
    defaultValues: { password: '', confirm: '' },
  });

  const submit = handleSubmit(async (v) => {
    setProblem(null);
    setDone(false);
    try {
      await unwrap(api.PUT('/v1/me/password', { body: { new_password: v.password } }));
    } catch (e) {
      const p = visibleProblem(e);
      setProblem(p && applyFieldErrors(p, { new_password: 'password' }, setError, t));
      return;
    }
    reset();
    setDone(true);
  });

  const err = (m?: string) => (m ? t(m, { min: PASSWORD_MIN, max: PASSWORD_MAX }) : undefined);
  return (
    <Section title={t('security.password.title')}>
      <form noValidate onSubmit={submit} className="flex flex-col gap-4">
        <p className="text-sm text-muted">{t('security.password.description')}</p>
        <TextField
          label={t('fields.new_password')}
          type="password"
          autoComplete="new-password"
          hint={t('register.password_hint', { min: PASSWORD_MIN, max: PASSWORD_MAX })}
          error={err(formState.errors.password?.message)}
          {...register('password')}
        />
        <TextField
          label={t('fields.confirm_password')}
          type="password"
          autoComplete="new-password"
          error={err(formState.errors.confirm?.message)}
          {...register('confirm')}
        />
        <ProblemAlert problem={problem} />
        <div aria-live="polite" className="text-sm">
          {done && <p>{t('security.password.done')}</p>}
        </div>
        <div>
          <Button type="submit" loading={formState.isSubmitting}>
            {t('security.password.submit')}
          </Button>
        </div>
      </form>
    </Section>
  );
}
