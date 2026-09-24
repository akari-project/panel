// SPDX-License-Identifier: AGPL-3.0-or-later
// 设置新密码（AUTH-04）。令牌在邮件链接的 URL 片段 #token= 中：读取后立即用 history.replaceState 清除，
// 避免留在地址栏、浏览历史与截图中。成功后服务端吊销该账号的全部会话。
import { zodResolver } from '@hookform/resolvers/zod';
import { useQueryClient } from '@tanstack/react-query';
import { Link, useRouteContext } from '@tanstack/react-router';
import { useEffect, useRef, useState } from 'react';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { isProblemError, unwrap, type Problem } from '@panel/sdk';
import { AuthLayout, Button, ProblemAlert, TextField, applyFieldErrors } from '@panel/ui';
import { newPassword, PASSWORD_MAX, PASSWORD_MIN, RESET_TOKEN } from '../forms';

const schema = z
  .object({ password: newPassword, confirm: z.string() })
  .refine((v) => v.password === v.confirm, { path: ['confirm'], error: 'validation.password_mismatch' });
type Values = z.infer<typeof schema>;

/** 从 URL 片段取出令牌；格式不对时返回 null。 */
export function readResetToken(hash: string): string | null {
  const token = new URLSearchParams(hash.replace(/^#/, '')).get('token');
  return token && RESET_TOKEN.test(token) ? token : null;
}

// 模块级缓存：清除片段后组件重新挂载（如 StrictMode）时仍能取得令牌。
let captured: { href: string; token: string | null } | null = null;

function captureToken(): string | null {
  const { hash, pathname, search } = window.location;
  if (hash) {
    captured = { href: pathname + search, token: readResetToken(hash) };
    window.history.replaceState(window.history.state, '', pathname + search);
  }
  return captured && captured.href === pathname + search ? captured.token : null;
}

/** 令牌已使用：之后再进入本页（没有片段）时不再沿用。 */
function forgetToken() {
  captured = null;
}

type TokenState = 'invalid' | 'expired' | null;

export function ResetPasswordPage() {
  const { t } = useTranslation();
  const { api, config } = useRouteContext({ from: '__root__' });
  const queryClient = useQueryClient();
  const [token] = useState(captureToken);
  const [tokenProblem, setTokenProblem] = useState<TokenState>(token ? null : 'invalid');
  const [problem, setProblem] = useState<Problem | null>(null);
  const [done, setDone] = useState(false);
  const statusRef = useRef<HTMLParagraphElement>(null);
  const { register, handleSubmit, setError, formState } = useForm<Values>({ resolver: zodResolver(schema) });

  useEffect(() => {
    if (done || tokenProblem) statusRef.current?.focus();
  }, [done, tokenProblem]);

  const submit = handleSubmit(async (v) => {
    if (!token) return;
    setProblem(null);
    try {
      await unwrap(api.POST('/v1/password-resets/confirmation', { body: { token, new_password: v.password } }));
    } catch (e) {
      if (!isProblemError(e)) throw e;
      const tokenError = e.problem.errors.find((x) => x.field === 'token');
      if (tokenError) {
        setTokenProblem(tokenError.code === 'expired' ? 'expired' : 'invalid');
        return;
      }
      setProblem(applyFieldErrors(e.problem, { new_password: 'password' }, setError, t));
      return;
    }
    // 服务端已吊销全部会话，丢弃缓存的账号信息。
    queryClient.clear();
    forgetToken();
    setDone(true);
  });

  const err = (m?: string) => (m ? t(m, { min: PASSWORD_MIN, max: PASSWORD_MAX }) : undefined);

  return (
    <AuthLayout siteName={config.site_name} sourceUrl={config.source_url}>
      <div className="flex flex-col gap-6">
        <h1 className="text-xl font-semibold">{t('reset.title')}</h1>
        {done ? (
          <div className="flex flex-col gap-4">
            <p ref={statusRef} tabIndex={-1} role="status">
              {t('reset.done')}
            </p>
            <Link to="/login" className="text-primary underline underline-offset-2">
              {t('reset.to_sign_in')}
            </Link>
          </div>
        ) : tokenProblem ? (
          <div className="flex flex-col gap-4">
            <p ref={statusRef} tabIndex={-1} role="alert" className="text-danger">
              {tokenProblem === 'expired' ? t('reset.link_expired') : t('reset.link_invalid')}
            </p>
            <Link to="/forgot-password" className="text-primary underline underline-offset-2">
              {t('reset.request_again')}
            </Link>
          </div>
        ) : (
          <form noValidate onSubmit={submit} className="flex flex-col gap-4">
            <TextField
              label={t('fields.new_password')}
              type="password"
              autoComplete="new-password"
              // eslint-disable-next-line jsx-a11y/no-autofocus
              autoFocus
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
            <Button type="submit" loading={formState.isSubmitting}>
              {t('reset.submit')}
            </Button>
          </form>
        )}
      </div>
    </AuthLayout>
  );
}
