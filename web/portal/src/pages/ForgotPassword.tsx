// SPDX-License-Identifier: AGPL-3.0-or-later
// 找回密码（AUTH-04）：无论邮箱是否存在，接口都返回 202，页面给出相同的提示。
import { zodResolver } from '@hookform/resolvers/zod';
import { Link, useRouteContext } from '@tanstack/react-router';
import { useEffect, useRef, useState } from 'react';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { isProblemError, unwrap, type Problem } from '@panel/sdk';
import { AuthLayout, Button, ProblemAlert, TextField, applyFieldErrors } from '@panel/ui';
import { email } from '../forms';

const schema = z.object({ email });
type Values = z.infer<typeof schema>;

export function ForgotPasswordPage() {
  const { t } = useTranslation();
  const { api, config } = useRouteContext({ from: '__root__' });
  const [problem, setProblem] = useState<Problem | null>(null);
  const [sentTo, setSentTo] = useState<string | null>(null);
  const doneRef = useRef<HTMLParagraphElement>(null);
  const { register, handleSubmit, setError, formState } = useForm<Values>({ resolver: zodResolver(schema) });

  useEffect(() => {
    if (sentTo) doneRef.current?.focus();
  }, [sentTo]);

  const submit = handleSubmit(async (v) => {
    setProblem(null);
    try {
      await unwrap(api.POST('/v1/password-resets', { body: { email: v.email } }));
      setSentTo(v.email);
    } catch (e) {
      if (!isProblemError(e)) throw e;
      setProblem(applyFieldErrors(e.problem, { email: 'email' }, setError, t));
    }
  });

  return (
    <AuthLayout siteName={config.site_name} sourceUrl={config.source_url}>
      <div className="flex flex-col gap-6">
        <h1 className="text-xl font-semibold">{t('forgot.title')}</h1>
        {sentTo ? (
          <p ref={doneRef} tabIndex={-1} role="status">
            {t('forgot.sent', { email: sentTo })}
          </p>
        ) : (
          <form noValidate onSubmit={submit} className="flex flex-col gap-4">
            <p className="text-sm text-muted">{t('forgot.hint')}</p>
            <TextField
              label={t('fields.email')}
              type="email"
              autoComplete="email"
              // eslint-disable-next-line jsx-a11y/no-autofocus
              autoFocus
              error={formState.errors.email?.message && t(formState.errors.email.message)}
              {...register('email')}
            />
            <ProblemAlert problem={problem} />
            <Button type="submit" loading={formState.isSubmitting}>
              {t('forgot.submit')}
            </Button>
          </form>
        )}
        <p className="text-sm">
          <Link to="/login" className="text-primary underline underline-offset-2">
            {t('forgot.back')}
          </Link>
        </p>
      </div>
    </AuthLayout>
  );
}
