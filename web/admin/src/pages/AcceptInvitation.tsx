// SPDX-License-Identifier: AGPL-3.0-or-later
// 接受管理员邀请（spec/10 AUTH-22）。邀请链接为 `accept-invitation#token=<token>`：
// 令牌在片段中，不进入服务端访问日志与 Referer；读取后立即从地址栏移除。
// 邮箱尚无账号（或账号未验证邮箱）时需要设置密码：服务端返回 errors[{field: password, code: required}]，
// 此时提示设置密码；已有账号时密码被忽略，用原密码登录。加入后首次登录时必须绑定 TOTP。
import { zodResolver } from '@hookform/resolvers/zod';
import { Link, useLocation, useNavigate, useRouteContext } from '@tanstack/react-router';
import { useEffect, useState } from 'react';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { isProblemError, unwrap, type Problem } from '@panel/sdk';
import { AuthLayout, Button, ProblemAlert, TextField, applyFieldErrors } from '@panel/ui';
import { newIdempotencyKey } from '../sensitive';

const PASSWORD_MIN = 8;
const PASSWORD_MAX = 128;

/** 从片段读取令牌：`#token=<token>`。 */
export function tokenFromHash(hash: string): string | null {
  const token = new URLSearchParams(hash.replace(/^#/, '')).get('token');
  return token?.trim() ? token.trim() : null;
}

const schema = z.object({
  password: z.union([
    z.literal(''),
    z.string().min(PASSWORD_MIN, { error: 'field_errors.too_short' }).max(PASSWORD_MAX, { error: 'field_errors.too_long' }),
  ]),
});

export function AcceptInvitationPage() {
  const { t } = useTranslation();
  const { config } = useRouteContext({ from: '__root__' });
  const hash = useLocation({ select: (l) => l.hash });
  const navigate = useNavigate();
  const fromHash = tokenFromHash(hash);
  const [token, setToken] = useState(fromHash);
  const [accepted, setAccepted] = useState(false);
  // 在本页再次打开另一个邀请链接时换用新令牌（渲染期间调整状态）。
  if (fromHash && fromHash !== token) {
    setToken(fromHash);
    setAccepted(false);
  }
  // 取得令牌后从地址栏移除片段。
  useEffect(() => {
    if (hash) void navigate({ to: '/accept-invitation', replace: true });
  }, [hash, navigate]);

  return (
    <AuthLayout siteName={config.site_name} sourceUrl={config.source_url}>
      <div className="flex flex-col gap-6">
        <h1 className="text-xl font-semibold">{t('accept.title')}</h1>
        {!token ? (
          <p role="alert" className="text-danger">
            {t('accept.missing_token')}
          </p>
        ) : accepted ? (
          <div className="flex flex-col gap-4">
            <p role="status">{t('accept.done')}</p>
            <Link to="/login" className="font-medium text-primary underline underline-offset-2">
              {t('accept.go_login')}
            </Link>
          </div>
        ) : (
          <AcceptForm key={token} token={token} onAccepted={() => setAccepted(true)} />
        )}
      </div>
    </AuthLayout>
  );
}

function AcceptForm({ token, onAccepted }: { token: string; onAccepted: () => void }) {
  const { t } = useTranslation();
  const tc = useTranslation('common').t;
  const { api } = useRouteContext({ from: '__root__' });
  const [problem, setProblem] = useState<Problem | null>(null);
  const [needsPassword, setNeedsPassword] = useState(false);
  const [idempotencyKey] = useState(newIdempotencyKey);
  const { register, handleSubmit, setError, formState } = useForm<{ password: string }>({
    resolver: zodResolver(schema),
    defaultValues: { password: '' },
  });

  const submit = handleSubmit(async ({ password }) => {
    setProblem(null);
    try {
      await unwrap(
        api.POST('/v1/staff-invitations/acceptance', {
          params: { header: { 'Idempotency-Key': idempotencyKey } },
          body: { token, ...(password ? { password } : {}) },
        }),
      );
    } catch (e) {
      if (!isProblemError(e)) throw e;
      const p = e.problem;
      if (p.errors.some((fe) => fe.field === 'password' && fe.code === 'required')) {
        setNeedsPassword(true);
        setError('password', { message: t('accept.password_required') }, { shouldFocus: true });
        return;
      }
      setProblem(p.errors.some((fe) => fe.field === 'token') ? p : applyFieldErrors(p, { password: 'password' }, setError, tc));
      return;
    }
    onAccepted();
  });

  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <p className="text-sm">{t('accept.intro')}</p>
      <TextField
        label={needsPassword ? t('accept.password_new') : t('accept.password_optional')}
        type="password"
        autoComplete="new-password"
        hint={t(needsPassword ? 'accept.password_hint_new' : 'accept.password_hint', { min: PASSWORD_MIN })}
        // 校验错误是 common 的翻译键；服务端要求设置密码时是已翻译的文案。
        error={formState.errors.password?.message && tc(formState.errors.password.message, { defaultValue: formState.errors.password.message })}
        {...register('password')}
      />
      <ProblemAlert problem={problem} message={problem ? acceptMessage(problem, t) : undefined} />
      <Button type="submit" loading={formState.isSubmitting}>
        {t('accept.submit')}
      </Button>
    </form>
  );
}

/** 令牌无效或过期（400 errors[token]）与状态不允许（409 invalid_state）使用专门的文案。 */
function acceptMessage(p: Problem, t: (k: string) => string): string | undefined {
  if (p.code === 'invalid_state') return t('accept.invalid_state');
  const token = p.errors.find((e) => e.field === 'token');
  if (token?.code === 'expired') return t('accept.expired');
  if (token || p.code === 'invalid_request') return t('accept.invalid_token');
  return undefined;
}
