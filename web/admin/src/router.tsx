// SPDX-License-Identifier: AGPL-3.0-or-later
// 管理后台的路由。未登录访问受保护页面时跳转到 /login，登录后回到原页面。
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
import { isProblemError, type ConsoleApi } from '@panel/sdk';
import { AppShell, ErrorState, navLinkClass, type PanelConfig } from '@panel/ui';
import { HomePage } from './pages/Home';
import { LoginPage } from './pages/Login';
import { NotFoundPage } from './pages/NotFound';
import { staffMeQuery } from './queries';

export interface RouterContext {
  queryClient: QueryClient;
  api: ConsoleApi;
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
        <li>
          <Link to="/" className={navLinkClass} activeOptions={{ exact: true }}>
            {t('nav.overview')}
          </Link>
        </li>
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
      await context.queryClient.ensureQueryData(staffMeQuery(context.api));
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

const routeTree = rootRoute.addChildren([loginRoute, appRoute.addChildren([homeRoute])]);

export function createAppRouter(context: RouterContext, history?: RouterHistory) {
  return createRouter({
    routeTree,
    context,
    basepath: context.config.base_path,
    defaultPreload: 'intent',
    ...(history ? { history } : {}),
  });
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>;
  }
}
