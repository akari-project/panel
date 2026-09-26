// SPDX-License-Identifier: AGPL-3.0-or-later
// 套餐（spec/32 32.2）：在售套餐对比、周期选择、可访问地区数。数据来自 GET /v1/plans（BIL-21：只含在售套餐与在售价格）。
// 金额与流量只做显示格式化（UI-04）。购买（报价、下单）在 M1-06 接入，此前购买按钮不可用。
import { useQuery } from '@tanstack/react-query';
import { useRouteContext } from '@tanstack/react-router';
import { useId, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { unwrap, type ClientSchemas } from '@panel/sdk';
import { Button, EmptyState, ErrorState, LoadingState, cn, formatBytes, formatMoney } from '@panel/ui';

type Plan = ClientSchemas['Plan'];
type Period = ClientSchemas['PlanPrice']['period'];

/** 周期选择只针对周期套餐；一次性套餐总是显示其一次性价格。 */
const recurringPeriods = ['month', 'quarter', 'half_year', 'year'] as const satisfies readonly Period[];
type RecurringPeriod = (typeof recurringPeriods)[number];

export function PlansPage() {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const q = useQuery({ queryKey: ['plans'], queryFn: () => unwrap(api.GET('/v1/plans')) });
  const [chosen, setChosen] = useState<RecurringPeriod | null>(null);

  const plans = q.data?.items ?? [];
  const periods = recurringPeriods.filter((p) => plans.some((plan) => plan.prices.some((pr) => pr.period === p)));
  const period = chosen && periods.includes(chosen) ? chosen : (periods[0] ?? null);

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-1">
        <h1 className="text-2xl font-semibold">{t('plans.title')}</h1>
        <p className="text-muted">{t('plans.intro')}</p>
      </div>
      {q.isPending ? (
        <LoadingState />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : plans.length === 0 ? (
        <EmptyState>{t('plans.empty')}</EmptyState>
      ) : (
        <>
          {periods.length > 1 && <PeriodPicker periods={periods} value={period} onChange={setChosen} />}
          <ul className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {plans.map((plan) => (
              <li key={plan.id}>
                <PlanCard plan={plan} period={period} />
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

/** 单选按钮组：方向键在选项间移动（原生 radio 行为），焦点样式可见（UI-05）。 */
function PeriodPicker({
  periods,
  value,
  onChange,
}: {
  periods: RecurringPeriod[];
  value: RecurringPeriod | null;
  onChange: (p: RecurringPeriod) => void;
}) {
  const { t } = useTranslation();
  const name = useId();
  return (
    <fieldset className="flex flex-col gap-2">
      <legend className="mb-1 text-sm font-medium">{t('plans.period_label')}</legend>
      <div className="flex flex-wrap gap-2">
        {periods.map((p) => (
          <label
            key={p}
            className={cn(
              'flex min-h-10 cursor-pointer items-center rounded-md border px-4 text-sm has-[:focus-visible]:outline-2 has-[:focus-visible]:outline-primary',
              value === p ? 'border-primary bg-primary text-primary-fg' : 'border-border bg-surface',
            )}
          >
            <input type="radio" name={name} value={p} checked={value === p} onChange={() => onChange(p)} className="sr-only" />
            {t(`plans.periods.${p}`)}
          </label>
        ))}
      </div>
    </fieldset>
  );
}

function PlanCard({ plan, period }: { plan: Plan; period: RecurringPeriod | null }) {
  const { t, i18n } = useTranslation();
  const titleId = useId();
  const price = plan.kind === 'one_time' ? plan.prices.find((p) => p.period === 'one_time') : plan.prices.find((p) => p.period === period);
  const facts: [string, string][] = [
    [t('plans.data'), plan.bytes_per_cycle === 0 ? t('plans.unlimited') : formatBytes(plan.bytes_per_cycle, i18n.language)],
    [t('plans.reset'), t(`plans.reset_policies.${plan.reset_policy}`)],
    [t('plans.devices'), t('plans.devices_value', { count: plan.device_limit })],
    [t('plans.speed'), plan.speed_limit_mbps ? t('plans.speed_value', { mbps: plan.speed_limit_mbps }) : t('plans.no_speed_limit')],
    [t('plans.locations'), String(plan.location_count)],
  ];
  return (
    <article aria-labelledby={titleId} className="flex h-full flex-col gap-4 rounded-lg border border-border bg-surface p-5">
      <div className="flex flex-col gap-1">
        <h2 id={titleId} className="text-lg font-semibold">
          {plan.name}
        </h2>
        {plan.description && <p className="text-sm text-muted">{plan.description}</p>}
      </div>
      <p className="text-2xl font-semibold">
        {plan.kind === 'free' ? (
          t('plans.free')
        ) : price ? (
          <>
            {formatMoney(price.amount_minor, price.currency, i18n.language)}
            <span className="ml-1 text-sm font-normal text-muted">
              {price.period === 'one_time'
                ? price.period_days
                  ? t('plans.valid_days', { count: price.period_days })
                  : t('plans.lifetime')
                : t(`plans.per.${price.period}`)}
            </span>
          </>
        ) : (
          <span className="text-base font-normal text-muted">{t('plans.period_unavailable')}</span>
        )}
      </p>
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
        {facts.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-muted">{k}</dt>
            <dd>{v}</dd>
          </div>
        ))}
      </dl>
      {plan.kind !== 'free' && (
        <div className="mt-auto">
          {/* 报价与下单在 M1-06 接入（spec/12）。 */}
          <Button className="w-full" disabled aria-describedby={`${titleId}-soon`}>
            {t('plans.buy')}
          </Button>
          <p id={`${titleId}-soon`} className="mt-1 text-center text-xs text-muted">
            {t('plans.buy_soon')}
          </p>
        </div>
      )}
    </article>
  );
}
