// SPDX-License-Identifier: AGPL-3.0-or-later
// 管理员登录必须完成二次验证（AUTH-21）：提交密码后接口总是返回 mfa_required，由 SignInFlow 进入第二步。
import { useQueryClient } from '@tanstack/react-query';
import { useNavigate, useRouteContext, useSearch } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { unwrap } from '@panel/sdk';
import { AuthLayout, SignInFlow, type MfaAnswer } from '@panel/ui';

export function LoginPage() {
  const { t } = useTranslation();
  const { api, config } = useRouteContext({ from: '__root__' });
  const search = useSearch({ from: '/login' });
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const onPassword = async (email: string, password: string) => {
    await unwrap(api.POST('/v1/sessions', { body: { email, password } }));
  };
  const onMfa = async (challenge_id: string, a: MfaAnswer) => {
    const body = a.method === 'totp' ? { challenge_id, totp_code: a.code } : { challenge_id, recovery_code: a.code };
    await unwrap(api.POST('/v1/sessions', { body }));
  };
  const onSignedIn = () => {
    void queryClient.invalidateQueries({ queryKey: ['staff', 'me'] });
    void navigate({ href: search.redirect ?? '/', replace: true });
  };

  return (
    <AuthLayout siteName={config.site_name} sourceUrl={config.source_url}>
      <SignInFlow title={t('login.title')} onPassword={onPassword} onMfa={onMfa} onSignedIn={onSignedIn} />
    </AuthLayout>
  );
}
