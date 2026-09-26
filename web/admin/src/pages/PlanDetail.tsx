// SPDX-License-Identifier: AGPL-3.0-or-later
// 套餐详情（spec/32 32.3）：编辑套餐、关联线路组、价格行。
// - 修改携带 If-Match（CONV-28），版本冲突时提示重新加载；
// - 修改等级或状态前、从套餐移除线路组前调用影响预览，显示“将影响 N 名用户”（UI-03、BIL-04、CON-07）；
// - 移除线路组是敏感操作（AUTH-19）：原因放在 Audit-Reason 请求头，带 Mfa-Assertion；
// - 价格行只能新建与停售（BIL-01），币种固定为站点结算货币（CONV-08）。
import { zodResolver } from '@hookform/resolvers/zod';
import { useQuery, useQueryClient, useSuspenseQuery } from '@tanstack/react-query';
import { Link, useNavigate, useRouteContext } from '@tanstack/react-router';
import { useMemo, useState, type ReactNode } from 'react';
import { useForm, useWatch } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { canOperate, isProblemError, unwrap, type Problem } from '@panel/sdk';
import {
  Button,
  ErrorState,
  LoadingState,
  Modal,
  ProblemAlert,
  SelectField,
  TextField,
  applyFieldErrors,
  formatDateTime,
  parseMoneyInput,
} from '@panel/ui';
import { ActionConfirm, useAsk } from '../components/Confirm';
import { CursorListView, TableFrame, td, th, useCursorList } from '../components/List';
import {
  allLocationGroupsQuery,
  isConflict,
  periodsFor,
  planQuery,
  plansKey,
  unwrapWithEtag,
  useFormatters,
  useSiteCurrency,
  type ImpactPreview,
  type LocationGroup,
  type Period,
  type Plan,
  type PlanPrice,
} from '../plans';
import { staffMeQuery } from '../queries';
import { SensitiveConfirm, auditReasonHeader, useIdempotencyKey, useSensitive } from '../sensitive';
import { PlanForm, changedFields, type PlanFields } from './PlanForm';

/** 操作成功的提示（屏幕阅读器通过 role=status 读出）。 */
function Notice({ text }: { text: string }) {
  return (
    <p role="status" className="rounded-md border border-border bg-surface p-3 text-sm">
      {text}
    </p>
  );
}

function Section({ title, actions, children }: { title: string; actions?: ReactNode; children: ReactNode }) {
  return (
    <section aria-label={title} className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-lg font-semibold">{title}</h2>
        {actions}
      </div>
      {children}
    </section>
  );
}

/** 影响预览的文案（UI-03）。 */
export function useImpactText() {
  const { t } = useTranslation();
  return (impact: ImpactPreview, extra?: string) =>
    [t('impact.accounts', { count: impact.affected_account_count }), t('impact.hosts', { count: impact.affected_host_count }), extra]
      .filter(Boolean)
      .join(' ');
}

export function PlanDetailPage({ planId }: { planId: string }) {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const q = useQuery(planQuery(api, planId));

  return (
    <div className="flex flex-col gap-8">
      <Link to="/plans" className="text-sm text-primary underline underline-offset-2">
        {t('plans.back')}
      </Link>
      {q.isPending ? (
        <LoadingState />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : (
        <PlanDetail plan={q.data.data} etag={q.data.etag} reload={() => void q.refetch()} />
      )}
    </div>
  );
}

function PlanDetail({ plan, etag, reload }: { plan: Plan; etag: string; reload: () => void }) {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const { data: me } = useSuspenseQuery(staffMeQuery(api));
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const f = useFormatters();
  const impactText = useImpactText();
  const [askElement, ask] = useAsk();
  const [notice, setNotice] = useState('');
  const [deleting, setDeleting] = useState(false);
  const canEdit = canOperate(me, 'PATCH /v1/plans/{id}');
  const canDelete = canOperate(me, 'DELETE /v1/plans/{id}');
  const key = planQuery(api, plan.id).queryKey;

  const save = async (fields: PlanFields) => {
    setNotice('');
    const patch = changedFields(plan, fields);
    if (Object.keys(patch).length === 0) {
      setNotice(t('plans.no_changes'));
      return;
    }
    // 等级或状态变化影响现有用户的访问与续费（BIL-21、ACS-05）：先预览影响再确认。
    if (patch.tier !== undefined || patch.status !== undefined) {
      const impact = await unwrap(
        api.POST('/v1/plans/{id}/impact', {
          params: { path: { id: plan.id } },
          body: { ...(patch.tier !== undefined ? { tier: patch.tier } : {}), ...(patch.status !== undefined ? { status: patch.status } : {}) },
        }),
      );
      const ok = await ask({
        title: t('plans.confirm_change_title'),
        description: impactText(impact, patch.status ? t(`plans.status_effects.${patch.status}`) : undefined),
        confirmLabel: t('plans.confirm_change'),
      });
      if (!ok) return;
    }
    const r = await unwrapWithEtag(
      api.PATCH('/v1/plans/{id}', { params: { path: { id: plan.id }, header: { 'If-Match': etag } }, body: patch }),
    );
    queryClient.setQueryData(key, r);
    await queryClient.invalidateQueries({ queryKey: [...plansKey, 'list'] });
    setNotice(t('plans.saved'));
  };

  const problemMessage = (p: Problem) =>
    isConflict(p) ? t('plans.conflict') : p.code === 'invalid_state' ? t('plans.update_invalid_state') : undefined;

  return (
    <>
      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex flex-col gap-1">
          <h1 className="text-2xl font-semibold">{plan.name}</h1>
          <p className="text-sm text-muted">
            {t('plans.summary', {
              kind: f.kind(plan.kind),
              status: f.status(plan.status),
              count: plan.active_entitlement_count,
            })}
          </p>
        </div>
        {canDelete && (
          <Button variant="secondary" onClick={() => setDeleting(true)}>
            {t('plans.delete')}
          </Button>
        )}
      </div>
      {notice && <Notice text={notice} />}
      <Section title={t('plans.basic')}>
        {canEdit ? (
          <div className="max-w-2xl">
            <PlanForm
              // ETag 变化（保存成功或重新加载）后以最新数据重建表单。
              key={etag}
              plan={plan}
              kindLocked={plan.prices.length > 0 || plan.active_entitlement_count > 0}
              submitLabel={t('plans.save')}
              onSubmit={save}
              problemMessage={problemMessage}
              problemAction={(p) =>
                isConflict(p) && (
                  <div>
                    <Button variant="secondary" onClick={reload}>
                      {t('plans.reload')}
                    </Button>
                  </div>
                )
              }
            />
          </div>
        ) : (
          <PlanFacts plan={plan} />
        )}
      </Section>
      <PlanGroups plan={plan} etag={etag} />
      {plan.kind === 'free' ? (
        <Section title={t('prices.title')}>
          <p className="text-sm text-muted">{t('prices.free_plan')}</p>
        </Section>
      ) : (
        <PlanPrices plan={plan} />
      )}
      {askElement}
      <ActionConfirm
        open={deleting}
        onOpenChange={setDeleting}
        title={t('plans.delete_title', { name: plan.name })}
        description={t('plans.delete_description')}
        confirmLabel={t('plans.delete')}
        danger
        problemMessage={(p) => (p.code === 'invalid_state' ? t('plans.delete_invalid_state') : isConflict(p) ? t('plans.conflict') : undefined)}
        onConfirm={async () => {
          await unwrap(api.DELETE('/v1/plans/{id}', { params: { path: { id: plan.id }, header: { 'If-Match': etag } } }));
          queryClient.removeQueries({ queryKey: key });
          await queryClient.invalidateQueries({ queryKey: [...plansKey, 'list'] });
          await navigate({ to: '/plans' });
        }}
      />
    </>
  );
}

/** 没有修改权限时的只读摘要。 */
function PlanFacts({ plan }: { plan: Plan }) {
  const { t } = useTranslation();
  const f = useFormatters();
  const rows: [string, string][] = [
    [t('plans.tier'), String(plan.tier)],
    [t('plans.bytes'), f.bytes(plan.bytes_per_cycle)],
    [t('plans.device_limit'), String(plan.device_limit)],
    [t('plans.speed_limit'), f.speed(plan.speed_limit_mbps)],
    [t('plans.reset_policy'), t(`plans.reset_policies.${plan.reset_policy}`)],
  ];
  return (
    <dl className="grid max-w-xl grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
      {rows.map(([k, v]) => (
        <div key={k} className="contents">
          <dt className="text-muted">{k}</dt>
          <dd>{v}</dd>
        </div>
      ))}
    </dl>
  );
}

function PlanGroups({ plan, etag }: { plan: Plan; etag: string }) {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const { data: me } = useSuspenseQuery(staffMeQuery(api));
  const queryClient = useQueryClient();
  const sensitive = useSensitive();
  const f = useFormatters();
  const impactText = useImpactText();
  const canList = canOperate(me, 'GET /v1/location-groups');
  const canAdd = canList && canOperate(me, 'PUT /v1/plans/{id}/location-groups/{group_id}');
  const canRemove = canOperate(me, 'DELETE /v1/plans/{id}/location-groups/{group_id}');
  const groups = useQuery({ ...allLocationGroupsQuery(api), enabled: canList });
  const [selected, setSelected] = useState('');
  const [busy, setBusy] = useState<string | null>(null);
  const [problem, setProblem] = useState<Problem | null>(null);
  const [removing, setRemoving] = useState<{ group: Pick<LocationGroup, 'id' | 'name'>; impact: ImpactPreview } | null>(null);
  const key = planQuery(api, plan.id).queryKey;

  const byId = new Map((groups.data ?? []).map((g) => [g.id, g]));
  const linked = plan.location_group_ids.map((id) => byId.get(id) ?? { id, name: id, min_tier: null });
  const available = (groups.data ?? []).filter((g) => !plan.location_group_ids.includes(g.id));

  const conflictMessage = (p: Problem) => (isConflict(p) ? t('plans.conflict_reloaded') : undefined);
  /** 版本冲突时重新读取套餐，之后的操作使用新的 ETag。 */
  const onError = async (e: unknown) => {
    if (isProblemError(e) && isConflict(e.problem)) await queryClient.invalidateQueries({ queryKey: key });
    throw e;
  };
  const updated = async (r: { data: Plan; etag: string }) => {
    queryClient.setQueryData(key, r);
    await queryClient.invalidateQueries({ queryKey: [...plansKey, 'list'] });
    await queryClient.invalidateQueries({ queryKey: ['location-groups'] });
  };

  // 添加线路组扩大访问，不需要影响确认（BIL-04）。
  const add = async () => {
    if (!selected) return;
    setBusy('add');
    setProblem(null);
    try {
      const r = await unwrapWithEtag(
        api.PUT('/v1/plans/{id}/location-groups/{group_id}', {
          params: { path: { id: plan.id, group_id: selected }, header: { 'If-Match': etag } },
        }),
      ).catch(onError);
      setSelected('');
      await updated(r);
    } catch (e) {
      if (!isProblemError(e)) throw e;
      setProblem(e.problem);
    } finally {
      setBusy(null);
    }
  };

  // 移除前预览影响（BIL-04）：拟设置的线路组集合为去掉该组后的其余各组。
  const prepareRemove = async (group: Pick<LocationGroup, 'id' | 'name'>) => {
    setBusy(group.id);
    setProblem(null);
    try {
      const impact = await unwrap(
        api.POST('/v1/plans/{id}/impact', {
          params: { path: { id: plan.id } },
          body: { location_group_ids: plan.location_group_ids.filter((id) => id !== group.id) },
        }),
      );
      setRemoving({ group, impact });
    } catch (e) {
      if (!isProblemError(e)) throw e;
      setProblem(e.problem);
    } finally {
      setBusy(null);
    }
  };

  return (
    <Section title={t('plans.groups')}>
      <p className="text-sm text-muted">{t('plans.groups_intro')}</p>
      {!canList && <p className="text-sm text-muted">{t('plans.groups_no_permission')}</p>}
      {canList && groups.isError && <ErrorState error={groups.error} onRetry={() => void groups.refetch()} />}
      {linked.length === 0 ? (
        <p className="text-sm">{t('plans.groups_empty')}</p>
      ) : (
        <ul className="flex flex-col divide-y divide-border rounded-lg border border-border">
          {linked.map((g) => (
            <li key={g.id} className="flex flex-col gap-2 p-3 sm:flex-row sm:items-center sm:justify-between">
              <div className="flex flex-col">
                <span className="font-medium">{g.name}</span>
                <span className="text-sm text-muted">{t('groups.min_tier_value', { value: f.minTier(g.min_tier) })}</span>
                {g.min_tier !== null && g.min_tier !== undefined && g.min_tier > plan.tier && (
                  <span className="text-sm text-danger">{t('plans.group_tier_warning', { tier: plan.tier, min: g.min_tier })}</span>
                )}
              </div>
              {canRemove && (
                <div>
                  <Button
                    variant="secondary"
                    loading={busy === g.id}
                    aria-label={t('plans.remove_group_for', { name: g.name })}
                    onClick={() => void prepareRemove(g)}
                  >
                    {t('plans.remove_group')}
                  </Button>
                </div>
              )}
            </li>
          ))}
        </ul>
      )}
      {canAdd && (
        <div className="flex max-w-lg flex-col gap-2 sm:flex-row sm:items-end">
          <div className="flex-1">
            <SelectField label={t('plans.add_group_label')} value={selected} onChange={(e) => setSelected(e.target.value)} disabled={available.length === 0}>
              <option value="">{available.length === 0 ? t('plans.no_more_groups') : t('plans.choose_group')}</option>
              {available.map((g) => (
                <option key={g.id} value={g.id}>
                  {g.name}
                </option>
              ))}
            </SelectField>
          </div>
          <Button onClick={() => void add()} loading={busy === 'add'} disabled={!selected}>
            {t('plans.add_group')}
          </Button>
        </div>
      )}
      <ProblemAlert problem={problem} message={problem ? conflictMessage(problem) : undefined} />
      <SensitiveConfirm
        open={!!removing}
        onOpenChange={(o) => !o && setRemoving(null)}
        title={t('plans.remove_group_title', { name: removing?.group.name ?? '' })}
        description={removing ? impactText(removing.impact, t('plans.remove_group_effect')) : ''}
        confirmLabel={t('plans.remove_group')}
        problemMessage={conflictMessage}
        onConfirm={async (reason) => {
          if (!removing) return;
          const r = await sensitive((assertion) =>
            unwrapWithEtag(
              api.DELETE('/v1/plans/{id}/location-groups/{group_id}', {
                params: {
                  path: { id: plan.id, group_id: removing.group.id },
                  header: { 'Mfa-Assertion': assertion, 'Audit-Reason': auditReasonHeader(reason), 'If-Match': etag },
                },
              }),
            ),
          ).catch(onError);
          await updated(r);
        }}
      />
    </Section>
  );
}

type SaleFilter = '' | 'true' | 'false';

function PlanPrices({ plan }: { plan: Plan }) {
  const { t, i18n } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const { data: me } = useSuspenseQuery(staffMeQuery(api));
  const queryClient = useQueryClient();
  const f = useFormatters();
  const [filter, setFilter] = useState<SaleFilter>('true');
  const [creating, setCreating] = useState(false);
  const [discontinuing, setDiscontinuing] = useState<PlanPrice | null>(null);
  const list = useCursorList<PlanPrice>([...plansKey, 'detail', plan.id, 'prices', filter], (cursor) =>
    unwrap(
      api.GET('/v1/plans/{id}/prices', {
        params: {
          path: { id: plan.id },
          query: { ...(filter ? { is_on_sale: filter === 'true' } : {}), ...(cursor ? { cursor } : {}) },
        },
      }),
    ),
  );
  const canCreate = canOperate(me, 'POST /v1/plans/{id}/prices');
  const canDiscontinue = canOperate(me, 'PATCH /v1/plans/{id}/prices/{price_id}');
  const refresh = async () => {
    await queryClient.invalidateQueries({ queryKey: [...plansKey, 'detail', plan.id] });
    await queryClient.invalidateQueries({ queryKey: [...plansKey, 'list'] });
  };

  return (
    <Section title={t('prices.title')} actions={canCreate && <Button onClick={() => setCreating(true)}>{t('prices.create')}</Button>}>
      <p className="text-sm text-muted">{t('prices.intro')}</p>
      <div className="max-w-xs">
        <SelectField label={t('prices.filter')} value={filter} onChange={(e) => setFilter(e.target.value as SaleFilter)}>
          <option value="true">{t('prices.on_sale')}</option>
          <option value="false">{t('prices.discontinued')}</option>
          <option value="">{t('plans.all')}</option>
        </SelectField>
      </div>
      <CursorListView list={list} empty={t('prices.empty')}>
        {(items) => (
          <TableFrame label={t('prices.title')}>
            <thead>
              <tr>
                <th className={th}>{t('prices.period')}</th>
                <th className={th}>{t('prices.validity')}</th>
                <th className={th}>{t('prices.amount')}</th>
                <th className={th}>{t('prices.state')}</th>
                <th className={th}>{t('prices.created_at')}</th>
                {canDiscontinue && <th className={th}>{t('list.actions')}</th>}
              </tr>
            </thead>
            <tbody>
              {items.map((p) => (
                <tr key={p.id}>
                  <td className={td}>{f.period(p.period)}</td>
                  <td className={td}>{f.validity(p)}</td>
                  <td className={td}>{f.money(p.amount_minor, p.currency)}</td>
                  <td className={td}>{p.is_on_sale ? t('prices.on_sale') : t('prices.discontinued')}</td>
                  <td className={td}>{formatDateTime(p.created_at, i18n.language)}</td>
                  {canDiscontinue && (
                    <td className={td}>
                      {p.is_on_sale && (
                        <Button
                          variant="ghost"
                          aria-label={t('prices.discontinue_for', { period: f.period(p.period), amount: f.money(p.amount_minor, p.currency) })}
                          onClick={() => setDiscontinuing(p)}
                        >
                          {t('prices.discontinue')}
                        </Button>
                      )}
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </TableFrame>
        )}
      </CursorListView>
      <Modal open={creating} onOpenChange={setCreating} title={t('prices.create')} description={t('prices.create_description')}>
        {creating && (
          <PriceForm
            plan={plan}
            onClose={() => setCreating(false)}
            onCreated={async () => {
              setCreating(false);
              await refresh();
            }}
          />
        )}
      </Modal>
      <ActionConfirm
        open={!!discontinuing}
        onOpenChange={(o) => !o && setDiscontinuing(null)}
        title={t('prices.discontinue_title')}
        description={
          discontinuing
            ? t('prices.discontinue_description', {
                period: f.period(discontinuing.period),
                amount: f.money(discontinuing.amount_minor, discontinuing.currency),
              })
            : ''
        }
        confirmLabel={t('prices.discontinue')}
        danger
        problemMessage={(p) => (p.code === 'invalid_state' ? t('prices.last_on_sale') : undefined)}
        onConfirm={async () => {
          if (!discontinuing) return;
          const path = { id: plan.id, price_id: discontinuing.id };
          // 价格行除 is_on_sale 外不可变，停售前读取当前 ETag（CONV-28）。
          const { etag } = await unwrapWithEtag(api.GET('/v1/plans/{id}/prices/{price_id}', { params: { path } }));
          await unwrap(
            api.PATCH('/v1/plans/{id}/prices/{price_id}', { params: { path, header: { 'If-Match': etag } }, body: { is_on_sale: false } }),
          );
          await refresh();
        }}
      />
    </Section>
  );
}

interface PriceValues {
  period: Period;
  period_days: string;
  amount: string;
}

function PriceForm({ plan, onClose, onCreated }: { plan: Plan; onClose: () => void; onCreated: () => Promise<void> }) {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const idempotencyKey = useIdempotencyKey();
  const [problem, setProblem] = useState<Problem | null>(null);
  const periods = periodsFor(plan.kind);
  const currency = useSiteCurrency();
  const schema = useMemo(
    () =>
      z
        .object({
          period: z.enum(periods as [Period, ...Period[]]),
          period_days: z.union([z.literal(''), z.string().trim().regex(/^[1-9]\d{0,5}$/, { error: 'plans.validation.positive_integer' })]),
          amount: z.string(),
        })
        .superRefine((v, ctx) => {
          if (!currency) return;
          const m = parseMoneyInput(v.amount, currency);
          if (!m.ok) ctx.addIssue({ code: 'custom', path: ['amount'], message: m.code === 'out_of_range' ? 'field_errors.out_of_range' : 'prices.amount_format' });
          else if (m.value <= 0) ctx.addIssue({ code: 'custom', path: ['amount'], message: 'prices.amount_positive' });
        }),
    [periods, currency],
  );
  const { register, control, handleSubmit, setError, formState } = useForm<PriceValues>({
    resolver: zodResolver(schema),
    defaultValues: { period: periods[0], period_days: '', amount: '' },
  });
  const period = useWatch({ control, name: 'period' });
  const err = (k: keyof PriceValues) => {
    const m = formState.errors[k]?.message;
    return m ? t(m) : undefined;
  };

  if (!currency) {
    // 站点尚未初始化结算货币时服务端同样拒绝新建（409 invalid_state）。
    return (
      <div className="flex flex-col gap-4">
        <p role="alert" className="text-sm text-danger">
          {t('prices.no_currency')}
        </p>
        <div className="flex justify-end">
          <Button variant="secondary" onClick={onClose}>
            {t('common:cancel')}
          </Button>
        </div>
      </div>
    );
  }

  const submit = handleSubmit(async (v) => {
    setProblem(null);
    const m = parseMoneyInput(v.amount, currency);
    if (!m.ok) return;
    const body = {
      period: v.period,
      amount_minor: m.value,
      currency,
      // period_days 只用于一次性套餐：为空表示长期有效（BIL-01、BIL-09）。
      ...(v.period === 'one_time' && v.period_days ? { period_days: Number(v.period_days) } : {}),
    };
    try {
      await unwrap(
        api.POST('/v1/plans/{id}/prices', {
          params: { path: { id: plan.id }, header: { 'Idempotency-Key': idempotencyKey(body) } },
          body,
        }),
      );
    } catch (e) {
      if (!isProblemError(e)) throw e;
      const left = applyFieldErrors(
        e.problem,
        { period: 'period', period_days: 'period_days', amount_minor: 'amount', currency: 'amount' },
        setError,
        t,
        (field, code) => t(`plans.field_errors.${field}.${code}`, { defaultValue: '' }) || undefined,
      );
      setProblem(left);
      return;
    }
    await onCreated();
  });

  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <SelectField label={t('prices.period')} error={err('period')} {...register('period')}>
        {periods.map((p) => (
          <option key={p} value={p}>
            {t(`plans.periods.${p}`)}
          </option>
        ))}
      </SelectField>
      {period === 'one_time' && (
        <TextField
          label={t('prices.period_days')}
          inputMode="numeric"
          hint={t('prices.period_days_hint')}
          error={err('period_days')}
          {...register('period_days')}
        />
      )}
      <TextField
        label={t('prices.amount_in', { currency })}
        inputMode="decimal"
        autoComplete="off"
        hint={t('prices.amount_hint')}
        error={err('amount')}
        {...register('amount')}
      />
      <ProblemAlert problem={problem} message={problem?.code === 'invalid_state' ? t('prices.no_currency') : undefined} />
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        <Button variant="secondary" onClick={onClose} disabled={formState.isSubmitting}>
          {t('common:cancel')}
        </Button>
        <Button type="submit" loading={formState.isSubmitting}>
          {t('prices.create_submit')}
        </Button>
      </div>
    </form>
  );
}
