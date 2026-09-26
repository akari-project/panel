// SPDX-License-Identifier: AGPL-3.0-or-later
// 管理后台的路由。未登录访问受保护页面时跳转到 /login，登录后回到原页面。
// 菜单与页面按当前管理员的权限显示（AUTH-17）：权限来自 GET /v1/staff/me，每个页面所需的权限取其列表接口的 x-permission；
// 直接访问无权限的页面时显示 forbidden 错误（服务端同样返回 403）。
import type { QueryClient } from '@tanstack/react-query';
import { useSuspenseQuery } from '@tanstack/react-query';
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
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { ProblemError, canOperate, isProblemError, toProblem, type ConsoleApi, type ConsoleOperation } from '@panel/sdk';
import { AppShell, ErrorState, navLinkClass, type PanelConfig } from '@panel/ui';
import { AcceptInvitationPage } from './pages/AcceptInvitation';
import { AuditLogsPage, validateAuditSearch } from './pages/AuditLogs';
import { HomePage } from './pages/Home';
import { LocationGroupsPage } from './pages/LocationGroups';
import { LoginPage } from './pages/Login';
import { NotFoundPage } from './pages/NotFound';
import { PlanDetailPage } from './pages/PlanDetail';
import { PlansPage } from './pages/Plans';
import { RolesPage } from './pages/Roles';
import { InvitationsPage, StaffPage } from './pages/Staff';
import { staffMeQuery } from './queries';
import type { StepUp } from './step-up';

export interface RouterContext {
  queryClient: QueryClient;
  api: ConsoleApi;
  config: PanelConfig;
  stepUp: StepUp;
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

// 邀请链接：`<ui.admin.public_url>accept-invitation#token=<token>`。令牌在片段中，不随请求发送到服务端。
export const acceptInvitationRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: 'accept-invitation',
  component: AcceptInvitationPage,
});

/** 菜单项：页面路径与决定是否显示的接口操作。 */
const navItems = [
  { to: '/plans', label: 'nav.plans', op: 'GET /v1/plans' },
  { to: '/location-groups', label: 'nav.location_groups', op: 'GET /v1/location-groups' },
  { to: '/staff', label: 'nav.staff', op: 'GET /v1/staff' },
  { to: '/roles', label: 'nav.roles', op: 'GET /v1/roles' },
  { to: '/audit-logs', label: 'nav.audit_logs', op: 'GET /v1/audit-logs' },
] as const satisfies readonly { to: string; label: string; op: ConsoleOperation }[];

function AppLayout() {
  const { t } = useTranslation();
  const { api, config, queryClient, stepUp } = useRouteContext({ from: appRoute.id });
  const { data: me } = useSuspenseQuery(staffMeQuery(api));
  const navigate = useNavigate();
  const signOut = async () => {
    // 登出失败（例如令牌已失效）也回到登录页。
    await api.DELETE('/v1/sessions/current').catch(() => undefined);
    stepUp.clear();
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
          {navItems
            .filter((item) => canOperate(me, item.op))
            .map((item) => (
              <li key={item.to}>
                <Link to={item.to} className={navLinkClass}>
                  {t(item.label)}
                </Link>
              </li>
            ))}
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

/** 页面级权限：没有 op 所需的权限时，在布局内显示 forbidden（与服务端的 403 一致），不渲染页面。 */
function guarded(op: ConsoleOperation, Page: () => ReactNode) {
  return function GuardedPage() {
    const { api } = useRouteContext({ from: '__root__' });
    const { data: me } = useSuspenseQuery(staffMeQuery(api));
    if (!canOperate(me, op)) {
      return <ErrorState error={new ProblemError(toProblem({ code: 'forbidden' }, new Response(null, { status: 403 })))} />;
    }
    return <Page />;
  };
}

export const homeRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/',
  component: HomePage,
});

export const staffRoute = createRoute({
  getParentRoute: () => appRoute,
  path: 'staff',
  component: guarded('GET /v1/staff', StaffPage),
});

export const invitationsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: 'staff/invitations',
  component: guarded('GET /v1/staff-invitations', InvitationsPage),
});

export const rolesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: 'roles',
  component: guarded('GET /v1/roles', RolesPage),
});

export const auditLogsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: 'audit-logs',
  validateSearch: validateAuditSearch,
  component: guarded('GET /v1/audit-logs', AuditLogsPage),
});

export const plansRoute = createRoute({
  getParentRoute: () => appRoute,
  path: 'plans',
  component: guarded('GET /v1/plans', PlansPage),
});

export const planDetailRoute = createRoute({
  getParentRoute: () => appRoute,
  path: 'plans/$planId',
  component: guarded('GET /v1/plans/{id}', function PlanDetailRoute() {
    const { planId } = planDetailRoute.useParams();
    return <PlanDetailPage planId={planId} />;
  }),
});

export const locationGroupsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: 'location-groups',
  component: guarded('GET /v1/location-groups', LocationGroupsPage),
});

const routeTree = rootRoute.addChildren([
  loginRoute,
  acceptInvitationRoute,
  appRoute.addChildren([
    homeRoute,
    plansRoute,
    planDetailRoute,
    locationGroupsRoute,
    staffRoute,
    invitationsRoute,
    rolesRoute,
    auditLogsRoute,
  ]),
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
 * 会话无法恢复（刷新令牌失效、空闲 30 分钟或 12 小时绝对失效，AUTH-21）时：
 * 当前在需要登录的页面上则跳转到登录页，登录后回到原页面，并丢弃缓存的管理员信息与 Mfa-Assertion。
 */
export function handleSessionExpired(router: ReturnType<typeof createAppRouter>, queryClient: QueryClient, stepUp: StepUp) {
  stepUp.clear();
  const { matches, location } = router.state;
  if (!matches.some((m) => m.routeId === appRoute.id)) return;
  void router
    .navigate({ to: '/login', search: { redirect: location.href }, replace: true })
    .then(() => queryClient.removeQueries({ queryKey: ['staff'] }));
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>;
  }
}
