// SPDX-License-Identifier: AGPL-3.0-or-later
// 邮箱验证（AUTH-03）：已登录时只提交验证码，未登录时提交邮箱与验证码。
// “重新发送”每分钟 1 次；服务端返回 429 时按 Retry-After 倒计时。
import { zodResolver } from '@hookform/resolvers/zod';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, useRouteContext, useSearch } from '@tanstack/react-router';
import { useEffect, useRef, useState } from 'react';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { isProblemError, unwrap, type Problem } from '@panel/sdk';
import { AuthLayout, Button, LoadingState, ProblemAlert, TextField, applyFieldErrors } from '@panel/ui';
import { email, emailCode, useCountdown } from '../forms';
import { meQuery } from '../queries';

const RESEND_INTERVAL = 60;

const signedInSchema = z.object({ email: z.string(), code: emailCode });
const anonymousSchema = z.object({ email, code: emailCode });
type Values = z.infer<typeof anonymousSchema>;

export function VerifyEmailPage() {
  const { api, config } = useRouteContext({ from: '__root__' });
  const { t } = useTranslation();
  // 未登录时 /v1/me 返回 401，视为未登录，不是错误。
  const me = useQuery({ ...meQuery(api), retry: false });
  // 验证成功后刷新账号信息时，仍显示“已验证”的结果，而不是“已经验证过了”。
  const [done, setDone] = useState(false);
  return (
    <AuthLayout siteName={config.site_name} sourceUrl={config.source_url}>
      <div className="flex flex-col gap-6">
        <h1 className="text-xl font-semibold">{t('verify.title')}</h1>
        {me.isPending ? (
          <LoadingState />
        ) : me.data?.is_email_verified && !done ? (
          <div className="flex flex-col gap-4">
            <p role="status">{t('verify.already')}</p>
            <Link to="/" className="text-primary underline underline-offset-2">
              {t('verify.to_overview')}
            </Link>
          </div>
        ) : (
          <VerifyForm signedIn={!!me.data} done={done} onDone={() => setDone(true)} />
        )}
      </div>
    </AuthLayout>
  );
}

function VerifyForm({ signedIn, done, onDone }: { signedIn: boolean; done: boolean; onDone: () => void }) {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const search = useSearch({ from: '/verify-email' });
  const queryClient = useQueryClient();
  const [problem, setProblem] = useState<Problem | null>(null);
  const [resendProblem, setResendProblem] = useState<Problem | null>(null);
  const [sent, setSent] = useState(false);
  const [resending, setResending] = useState(false);
  const cooldown = useCountdown();
  const doneRef = useRef<HTMLParagraphElement>(null);

  const { register, handleSubmit, setError, formState, trigger, getValues, setFocus } = useForm<Values>({
    resolver: zodResolver(signedIn ? signedInSchema : anonymousSchema),
    defaultValues: { email: search.email ?? '', code: '' },
  });

  // 刚注册过来时验证邮件刚发出，按发送间隔开始倒计时。
  const startCooldown = cooldown.start;
  useEffect(() => {
    if (search.email) startCooldown(RESEND_INTERVAL);
  }, [search.email, startCooldown]);

  useEffect(() => {
    setFocus(signedIn || search.email ? 'code' : 'email');
  }, [signedIn, search.email, setFocus]);

  useEffect(() => {
    if (done) doneRef.current?.focus();
  }, [done]);

  const submit = handleSubmit(async (v) => {
    setProblem(null);
    try {
      await unwrap(api.POST('/v1/accounts/verification', { body: signedIn ? { code: v.code } : { email: v.email, code: v.code } }));
    } catch (e) {
      if (!isProblemError(e)) throw e;
      setProblem(applyFieldErrors(e.problem, { code: 'code', email: 'email' }, setError, t));
      return;
    }
    onDone();
    if (signedIn) await queryClient.invalidateQueries({ queryKey: ['me'] });
  });

  const resend = async () => {
    setResendProblem(null);
    setSent(false);
    if (!signedIn && !(await trigger('email'))) return;
    setResending(true);
    try {
      await unwrap(api.POST('/v1/accounts/verification/resend', { body: signedIn ? {} : { email: getValues('email') } }));
      setSent(true);
      cooldown.start(RESEND_INTERVAL);
    } catch (e) {
      if (!isProblemError(e)) throw e;
      if (e.problem.status === 429) cooldown.start(e.problem.retryAfter ?? RESEND_INTERVAL);
      setResendProblem(applyFieldErrors(e.problem, { email: 'email' }, setError, t));
    } finally {
      setResending(false);
    }
  };

  if (done) {
    return (
      <div className="flex flex-col gap-4">
        <p ref={doneRef} tabIndex={-1} role="status">
          {t('verify.done')}
        </p>
        {signedIn ? (
          <Link to="/" className="text-primary underline underline-offset-2">
            {t('verify.to_overview')}
          </Link>
        ) : (
          <Link to="/login" className="text-primary underline underline-offset-2">
            {t('verify.to_sign_in')}
          </Link>
        )}
      </div>
    );
  }

  const fieldError = (m?: string) => (m ? t(m) : undefined);
  const address = signedIn ? undefined : search.email;
  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <p className="text-sm text-muted">{address ? t('verify.hint_sent', { email: address }) : t('verify.hint')}</p>
      {!signedIn && (
        <TextField label={t('fields.email')} type="email" autoComplete="email" error={fieldError(formState.errors.email?.message)} {...register('email')} />
      )}
      <TextField
        label={t('verify.code')}
        inputMode="numeric"
        autoComplete="one-time-code"
        maxLength={6}
        error={fieldError(formState.errors.code?.message)}
        {...register('code')}
      />
      <ProblemAlert problem={problem} />
      <Button type="submit" loading={formState.isSubmitting}>
        {t('verify.submit')}
      </Button>
      <div className="flex flex-col gap-2">
        <Button variant="secondary" onClick={() => void resend()} loading={resending} disabled={cooldown.remaining > 0}>
          {cooldown.remaining > 0 ? t('verify.resend_in', { seconds: cooldown.remaining }) : t('verify.resend')}
        </Button>
        <div aria-live="polite" className="text-sm">
          {sent && <p className="text-muted">{t('verify.resent')}</p>}
        </div>
        <ProblemAlert problem={resendProblem} />
      </div>
      {!signedIn && (
        <p className="text-sm text-muted">
          <Link to="/login" className="text-primary underline underline-offset-2">
            {t('verify.to_sign_in')}
          </Link>
        </p>
      )}
    </form>
  );
}
