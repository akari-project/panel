// SPDX-License-Identifier: AGPL-3.0-or-later
import { useQueryClient } from '@tanstack/react-query';
import { useNavigate, useRouteContext, useSearch } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { unwrap, type ClientSchemas } from '@panel/sdk';
import { AuthLayout, SignInFlow, type MfaAnswer } from '@panel/ui';

// 用户中心以 web 设备登录：令牌以 HttpOnly Cookie 下发，不生成代理凭据（AUTH-08、AUTH-10）。
const device: ClientSchemas['DeviceInfo'] = { platform: 'web' };

export function LoginPage() {
  const { t } = useTranslation();
  const { api, config } = useRouteContext({ from: '__root__' });
  const search = useSearch({ from: '/login' });
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const onPassword = async (email: string, password: string) => {
    await unwrap(api.POST('/v1/sessions', { body: { email, password, device } }));
  };
  const onMfa = async (challenge_id: string, a: MfaAnswer) => {
    const body =
      a.method === 'totp' ? { challenge_id, device, totp_code: a.code } : { challenge_id, device, recovery_code: a.code };
    await unwrap(api.POST('/v1/sessions', { body }));
  };
  const onSignedIn = () => {
    void queryClient.invalidateQueries({ queryKey: ['me'] });
    void navigate({ href: search.redirect ?? '/', replace: true });
  };

  return (
    <AuthLayout siteName={config.site_name} sourceUrl={config.source_url}>
      <SignInFlow title={t('login.title')} onPassword={onPassword} onMfa={onMfa} onSignedIn={onSignedIn} />
    </AuthLayout>
  );
}
