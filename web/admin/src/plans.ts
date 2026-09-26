// SPDX-License-Identifier: AGPL-3.0-or-later
// 套餐、价格行与线路组页面共用的查询与取值（spec/11、spec/31）。
import { queryOptions, useQuery, useSuspenseQuery } from '@tanstack/react-query';
import { useRouteContext } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { ProblemError, canOperate, toProblem, unwrap, type ConsoleApi, type ConsoleSchemas, type Problem } from '@panel/sdk';
import { formatBytes, formatMoney } from '@panel/ui';
import { staffMeQuery } from './queries';

export type Plan = ConsoleSchemas['Plan'];
export type PlanPrice = ConsoleSchemas['PlanPrice'];
export type LocationGroup = ConsoleSchemas['LocationGroup'];
export type ImpactPreview = ConsoleSchemas['ImpactPreview'];

export const planStatuses = ['draft', 'on_sale', 'hidden', 'archived'] as const;
export const planKinds = ['recurring', 'one_time', 'free'] as const;
export const resetPolicies = ['purchase_anchor', 'calendar_month', 'never'] as const;
export type PlanStatus = (typeof planStatuses)[number];
export type PlanKind = (typeof planKinds)[number];
export type Period = PlanPrice['period'];

/** 周期必须与套餐类型匹配（BIL-01）。免费套餐不设价格行。 */
const periodsByKind: Record<PlanKind, readonly Period[]> = {
  recurring: ['month', 'quarter', 'half_year', 'year'],
  one_time: ['one_time'],
  free: [],
};

export function periodsFor(kind: PlanKind): readonly Period[] {
  return periodsByKind[kind];
}

export const plansKey = ['plans'] as const;
export const locationGroupsKey = ['location-groups'] as const;

/** 把 openapi-fetch 的结果转换为数据与 ETag（CONV-13），用于之后的 If-Match（CONV-28）。 */
export async function unwrapWithEtag<T>(
  request: Promise<{ data?: T; error?: unknown; response: Response }>,
): Promise<{ data: T; etag: string }> {
  let r: { data?: T; error?: unknown; response: Response };
  try {
    r = await request;
  } catch {
    throw new ProblemError(toProblem(undefined));
  }
  if (!r.response.ok || r.data === undefined) throw new ProblemError(toProblem(r.error, r.response));
  return { data: r.data, etag: r.response.headers.get('ETag') ?? '' };
}

export const planQuery = (api: ConsoleApi, id: string) =>
  queryOptions({
    queryKey: [...plansKey, 'detail', id],
    queryFn: () => unwrapWithEtag(api.GET('/v1/plans/{id}', { params: { path: { id } } })),
  });

// 取全部线路组时的页数上限，防止异常的游标导致无限请求。
const MAX_PAGES = 50;

/** 全部线路组（供套餐关联时选择与显示名称）：按游标取完全部页，每页 200 条（CONV-11）。 */
export const allLocationGroupsQuery = (api: ConsoleApi) =>
  queryOptions({
    queryKey: [...locationGroupsKey, 'all'],
    queryFn: async () => {
      const items: LocationGroup[] = [];
      let cursor: string | undefined;
      for (let i = 0; i < MAX_PAGES; i++) {
        const page = await unwrap(api.GET('/v1/location-groups', { params: { query: { limit: 200, ...(cursor ? { cursor } : {}) } } }));
        items.push(...page.items);
        cursor = page.next_cursor ?? undefined;
        if (!cursor) break;
      }
      return items;
    },
  });

/**
 * 站点结算货币（CONV-08），新建价格行时固定使用。来自 GET /v1/settings（需要 settings.read）；
 * 没有该权限时取已有价格行的币种（同一站点只有一种货币，BIL-01）。都取不到时为 undefined。
 */
export function useSiteCurrency(known?: string): { currency: string | undefined; isPending: boolean } {
  const { api } = useRouteContext({ from: '__root__' });
  const { data: me } = useSuspenseQuery(staffMeQuery(api));
  const canRead = canOperate(me, 'GET /v1/settings');
  const settings = useQuery({
    queryKey: ['settings'],
    queryFn: () => unwrap(api.GET('/v1/settings')),
    enabled: canRead,
    staleTime: 5 * 60_000,
  });
  const probe = useQuery({
    queryKey: [...plansKey, 'currency-probe'],
    queryFn: async () => {
      const page = await unwrap(api.GET('/v1/plans', { params: { query: { limit: 200 } } }));
      return page.items.flatMap((p) => p.prices)[0]?.currency ?? null;
    },
    enabled: !known && (!canRead || settings.isError),
    staleTime: 5 * 60_000,
  });
  const currency = settings.data?.currency ?? known ?? probe.data ?? undefined;
  const isPending = !currency && ((canRead && settings.isPending) || probe.isFetching);
  return { currency, isPending };
}

export function useFormatters() {
  const { t, i18n } = useTranslation();
  return {
    bytes: (b: number) => (b === 0 ? t('plans.unlimited') : formatBytes(b, i18n.language)),
    speed: (mbps: number | null | undefined) => (mbps ? t('plans.speed_value', { mbps }) : t('plans.no_speed_limit')),
    money: (minor: number, currency: string) => formatMoney(minor, currency, i18n.language),
    period: (p: Period) => t(`plans.periods.${p}`),
    validity: (p: Pick<PlanPrice, 'period' | 'period_days'>) =>
      p.period !== 'one_time' ? '—' : p.period_days ? t('plans.days', { count: p.period_days }) : t('plans.lifetime'),
    status: (s: PlanStatus) => t(`plans.statuses.${s}`),
    kind: (k: PlanKind) => t(`plans.kinds.${k}`),
    minTier: (v: number | null | undefined) => (v === null || v === undefined ? t('groups.no_min_tier') : String(v)),
  };
}

/** 版本冲突（CONV-28）：数据已被他人修改。 */
export const isConflict = (p: Problem) => p.status === 409 && p.code === 'conflict';
