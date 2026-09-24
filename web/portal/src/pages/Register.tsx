// SPDX-License-Identifier: AGPL-3.0-or-later
// 注册（AUTH-01、AUTH-02）。/v1/config 尚未提供注册策略时，邀请码始终显示为可选；
// 注册关闭或策略要求邀请码而未填写时，服务端返回 403 registration_closed。
import { zodResolver } from '@hookform/resolvers/zod';
import { Link, useNavigate, useRouteContext } from '@tanstack/react-router';
import { useState } from 'react';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { isProblemError, unwrap, type Problem } from '@panel/sdk';
import { AuthLayout, Button, ProblemAlert, TextField, applyFieldErrors } from '@panel/ui';
import { email, newPassword, PASSWORD_MAX, PASSWORD_MIN } from '../forms';

const schema = z.object({
  email,
  password: newPassword,
  invite_code: z.string().trim().max(64, { error: 'validation.too_long' }),
});
type Values = z.infer<typeof schema>;

function browserTimezone(): string | undefined {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || undefined;
  } catch {
    return undefined;
  }
}

export function RegisterPage() {
  const { t, i18n } = useTranslation();
  const { api, config } = useRouteContext({ from: '__root__' });
  const navigate = useNavigate();
  const [problem, setProblem] = useState<Problem | null>(null);
  const { register, handleSubmit, setError, formState } = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { email: '', password: '', invite_code: '' },
  });

  const submit = handleSubmit(async (v) => {
    setProblem(null);
    const timezone = browserTimezone();
    try {
      await unwrap(
        api.POST('/v1/accounts', {
          body: {
            email: v.email,
            password: v.password,
            ...(v.invite_code ? { invite_code: v.invite_code } : {}),
            locale: i18n.resolvedLanguage ?? i18n.language,
            ...(timezone ? { timezone } : {}),
          },
        }),
      );
    } catch (e) {
      if (!isProblemError(e)) throw e;
      setProblem(applyFieldErrors(e.problem, { email: 'email', password: 'password', invite_code: 'invite_code' }, setError, t));
      return;
    }
    // 已注册的邮箱同样返回 202（防枚举），页面不作区分。
    await navigate({ to: '/verify-email', search: { email: v.email } });
  });

  const err = (m?: string) => (m ? t(m, { min: PASSWORD_MIN, max: PASSWORD_MAX }) : undefined);

  return (
    <AuthLayout siteName={config.site_name} sourceUrl={config.source_url}>
      <div className="flex flex-col gap-6">
        <h1 className="text-xl font-semibold">{t('register.title')}</h1>
        <form noValidate onSubmit={submit} className="flex flex-col gap-4">
          <TextField
            label={t('fields.email')}
            type="email"
            autoComplete="email"
            // 注册页的唯一任务是填写表单，自动聚焦符合预期。
            // eslint-disable-next-line jsx-a11y/no-autofocus
            autoFocus
            error={err(formState.errors.email?.message)}
            {...register('email')}
          />
          <TextField
            label={t('fields.password')}
            type="password"
            autoComplete="new-password"
            hint={t('register.password_hint', { min: PASSWORD_MIN, max: PASSWORD_MAX })}
            error={err(formState.errors.password?.message)}
            {...register('password')}
          />
          <TextField
            label={t('register.invite_code')}
            autoComplete="off"
            hint={t('register.invite_code_hint')}
            error={err(formState.errors.invite_code?.message)}
            {...register('invite_code')}
          />
          <ProblemAlert
            problem={problem}
            {...(problem?.code === 'registration_closed' ? { message: t('register.closed') } : {})}
          />
          <Button type="submit" loading={formState.isSubmitting}>
            {t('register.submit')}
          </Button>
        </form>
        <p className="text-sm text-muted">
          {t('register.have_account')}{' '}
          <Link to="/login" className="text-primary underline underline-offset-2">
            {t('register.sign_in')}
          </Link>
        </p>
      </div>
    </AuthLayout>
  );
}
