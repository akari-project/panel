// SPDX-License-Identifier: AGPL-3.0-or-later
// 用户中心的路由。未登录访问受保护页面时跳转到 /login，登录后回到原页面。
import type { QueryClient } from '@tanstack/react-query';
import {
  Link,
  Outlet,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  redirect,
  useNavigate,
  useRouteContext,
  useRouter,
  type RouterHistory,
} from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { isProblemError, type ClientApi } from '@panel/sdk';
import { AppShell, ErrorState, navLinkClass, type PanelConfig } from '@panel/ui';
import { DevicesPage } from './pages/Devices';
import { ForgotPasswordPage } from './pages/ForgotPassword';
import { HomePage } from './pages/Home';
import { LoginPage } from './pages/Login';
import { NotFoundPage } from './pages/NotFound';
import { PlansPage } from './pages/Plans';
import { RegisterPage } from './pages/Register';
import { ResetPasswordPage } from './pages/ResetPassword';
import { SecurityPage } from './pages/Security';
import { VerifyEmailPage } from './pages/VerifyEmail';
import { meQuery } from './queries';

export interface RouterContext {
  queryClient: QueryClient;
  api: ClientApi;
  config: PanelConfig;
}

/** 只接受站内路径，防止登录后跳转到外部地址。 */
export function safeRedirect(target: unknown): string | undefined {
  return typeof target === 'string' && target.startsWith('/') && !target.startsWith('//') ? target : undefined;
}

const rootRoute = createRootRouteWithContext<RouterContext>()({
  component: Outlet,
  notFoundComponent: NotFoundPage,
});

export const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: 'login',
  validateSearch: (s: Record<string, unknown>): { redirect?: string } => {
    const r = safeRedirect(s.redirect);
    return r ? { redirect: r } : {};
  },
  component: LoginPage,
});

export const registerRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: 'register',
  component: RegisterPage,
});

// 未登录与已登录都可以访问：已登录时只需验证码（AUTH-03）。
export const verifyEmailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: 'verify-email',
  validateSearch: (s: Record<string, unknown>): { email?: string } =>
    typeof s.email === 'string' && s.email.length <= 254 ? { email: s.email } : {},
  component: VerifyEmailPage,
});

export const forgotPasswordRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: 'forgot-password',
  component: ForgotPasswordPage,
});

// 令牌在 URL 片段中（AUTH-04），由页面读取并清除，不作为查询参数。
export const resetPasswordRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: 'reset-password',
  component: ResetPasswordPage,
});

function AppLayout() {
  const { t } = useTranslation();
  const { api, config, queryClient } = useRouteContext({ from: appRoute.id });
  const navigate = useNavigate();
  const signOut = async () => {
    // 登出失败（例如令牌已失效）也回到登录页。
    await api.DELETE('/v1/sessions/current').catch(() => undefined);
    queryClient.clear();
    await navigate({ to: '/login' });
  };
  return (
    <AppShell
      siteName={config.site_name}
      sourceUrl={config.source_url}
      onSignOut={() => void signOut()}
      nav={
        <>
          <li>
            <Link to="/" className={navLinkClass} activeOptions={{ exact: true }}>
              {t('nav.overview')}
            </Link>
          </li>
          <li>
            <Link to="/plans" className={navLinkClass}>
              {t('nav.plans')}
            </Link>
          </li>
          <li>
            <Link to="/devices" className={navLinkClass}>
              {t('nav.devices')}
            </Link>
          </li>
          <li>
            <Link to="/security" className={navLinkClass}>
              {t('nav.security')}
            </Link>
          </li>
        </>
      }
    >
      <Outlet />
    </AppShell>
  );
}

function RouteError({ error, reset }: { error: unknown; reset: () => void }) {
  const router = useRouter();
  return (
    <div className="p-6">
      <ErrorState
        error={error}
        onRetry={() => {
          reset();
          void router.invalidate();
        }}
      />
    </div>
  );
}

const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: 'app',
  beforeLoad: async ({ context, location }) => {
    try {
      await context.queryClient.ensureQueryData(meQuery(context.api));
    } catch (e) {
      if (isProblemError(e) && e.problem.status === 401) {
        throw redirect({ to: '/login', search: { redirect: location.href } });
      }
      throw e;
    }
  },
  component: AppLayout,
  errorComponent: RouteError,
});

export const homeRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/',
  component: HomePage,
});

export const plansRoute = createRoute({
  getParentRoute: () => appRoute,
  path: 'plans',
  component: PlansPage,
});

export const devicesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: 'devices',
  component: DevicesPage,
});

export const securityRoute = createRoute({
  getParentRoute: () => appRoute,
  path: 'security',
  component: SecurityPage,
});

const routeTree = rootRoute.addChildren([
  loginRoute,
  registerRoute,
  verifyEmailRoute,
  forgotPasswordRoute,
  resetPasswordRoute,
  appRoute.addChildren([homeRoute, plansRoute, devicesRoute, securityRoute]),
]);

export function createAppRouter(context: RouterContext, history?: RouterHistory) {
  return createRouter({
    routeTree,
    context,
    basepath: context.config.base_path,
    defaultPreload: 'intent',
    ...(history ? { history } : {}),
  });
}

/**
 * 会话无法恢复（刷新令牌失效）时：当前在需要登录的页面上则跳转到登录页，登录后回到原页面，并丢弃缓存的账号信息。
 * 公开页面（注册、找回密码等）与首次加载（由 appRoute.beforeLoad 处理）不在这里跳转。
 */
export function handleSessionExpired(router: ReturnType<typeof createAppRouter>, queryClient: QueryClient) {
  const { matches, location } = router.state;
  if (!matches.some((m) => m.routeId === appRoute.id)) return;
  void router
    .navigate({ to: '/login', search: { redirect: location.href }, replace: true })
    .then(() => queryClient.removeQueries({ queryKey: ['me'] }));
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>;
  }
}
