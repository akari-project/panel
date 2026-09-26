// SPDX-License-Identifier: AGPL-3.0-or-later
// 套餐列表（spec/32 32.3“套餐与价格”）：按状态与类型筛选，游标分页（CONV-11，服务端按 sort、id 排序）。
// 新建的套餐为草稿（BIL-26），价格行与线路组在详情页中添加。
import { useQueryClient, useSuspenseQuery } from '@tanstack/react-query';
import { Link, useNavigate, useRouteContext } from '@tanstack/react-router';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { canOperate, unwrap } from '@panel/sdk';
import { Button, Modal, SelectField } from '@panel/ui';
import { CursorListView, PageHeader, TableFrame, td, th, useCursorList } from '../components/List';
import { plansKey, planKinds, planStatuses, useFormatters, type Plan, type PlanKind, type PlanStatus } from '../plans';
import { staffMeQuery } from '../queries';
import { useIdempotencyKey } from '../sensitive';
import { PlanForm, type PlanFields } from './PlanForm';

export function PlansPage() {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const { data: me } = useSuspenseQuery(staffMeQuery(api));
  const f = useFormatters();
  const [status, setStatus] = useState<PlanStatus | ''>('');
  const [kind, setKind] = useState<PlanKind | ''>('');
  const [creating, setCreating] = useState(false);
  const list = useCursorList<Plan>([...plansKey, 'list', status, kind], (cursor) =>
    unwrap(
      api.GET('/v1/plans', {
        params: { query: { ...(status ? { status } : {}), ...(kind ? { kind } : {}), ...(cursor ? { cursor } : {}) } },
      }),
    ),
  );
  const canCreate = canOperate(me, 'POST /v1/plans');

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title={t('plans.title')} actions={canCreate && <Button onClick={() => setCreating(true)}>{t('plans.create')}</Button>}>
        <p className="text-sm text-muted">{t('plans.intro')}</p>
      </PageHeader>
      <div className="grid max-w-lg gap-4 sm:grid-cols-2">
        <SelectField label={t('plans.status')} value={status} onChange={(e) => setStatus(e.target.value as PlanStatus | '')}>
          <option value="">{t('plans.all')}</option>
          {planStatuses.map((s) => (
            <option key={s} value={s}>
              {f.status(s)}
            </option>
          ))}
        </SelectField>
        <SelectField label={t('plans.kind')} value={kind} onChange={(e) => setKind(e.target.value as PlanKind | '')}>
          <option value="">{t('plans.all')}</option>
          {planKinds.map((k) => (
            <option key={k} value={k}>
              {f.kind(k)}
            </option>
          ))}
        </SelectField>
      </div>
      <CursorListView list={list} empty={t('plans.empty')}>
        {(items) => (
          <TableFrame label={t('plans.title')}>
            <thead>
              <tr>
                <th className={th}>{t('plans.name')}</th>
                <th className={th}>{t('plans.kind')}</th>
                <th className={th}>{t('plans.tier')}</th>
                <th className={th}>{t('plans.status')}</th>
                <th className={th}>{t('plans.bytes')}</th>
                <th className={th}>{t('plans.device_limit')}</th>
                <th className={th}>{t('plans.prices_on_sale')}</th>
                <th className={th}>{t('plans.active_entitlements')}</th>
                <th className={th}>{t('plans.sort')}</th>
              </tr>
            </thead>
            <tbody>
              {items.map((p) => (
                <tr key={p.id}>
                  <td className={td}>
                    <Link to="/plans/$planId" params={{ planId: p.id }} className="font-medium text-primary underline underline-offset-2">
                      {p.name}
                    </Link>
                  </td>
                  <td className={td}>{f.kind(p.kind)}</td>
                  <td className={td}>{p.tier}</td>
                  <td className={td}>{f.status(p.status)}</td>
                  <td className={td}>{f.bytes(p.bytes_per_cycle)}</td>
                  <td className={td}>{p.device_limit}</td>
                  <td className={td}>
                    {p.prices.length === 0
                      ? '—'
                      : p.prices.map((pr) => (
                          <div key={pr.id} className="whitespace-nowrap">
                            {f.period(pr.period)} {f.money(pr.amount_minor, pr.currency)}
                          </div>
                        ))}
                  </td>
                  <td className={td}>{p.active_entitlement_count}</td>
                  <td className={td}>{p.sort}</td>
                </tr>
              ))}
            </tbody>
          </TableFrame>
        )}
      </CursorListView>
      <CreatePlanDialog open={creating} onClose={() => setCreating(false)} />
    </div>
  );
}

function CreatePlanDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { t } = useTranslation();
  return (
    <Modal open={open} onOpenChange={(o) => !o && onClose()} title={t('plans.create')} description={t('plans.create_description')}>
      {open && <CreatePlanForm onClose={onClose} />}
    </Modal>
  );
}

function CreatePlanForm({ onClose }: { onClose: () => void }) {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const idempotencyKey = useIdempotencyKey();
  const submit = async (fields: PlanFields) => {
    // 新建的套餐总是草稿：非免费套餐须先有在售价格才能上架（BIL-26）。
    const { status: _status, ...body } = fields;
    void _status;
    const plan = await unwrap(api.POST('/v1/plans', { params: { header: { 'Idempotency-Key': idempotencyKey(body) } }, body }));
    await queryClient.invalidateQueries({ queryKey: plansKey });
    onClose();
    await navigate({ to: '/plans/$planId', params: { planId: plan.id } });
  };
  return <PlanForm submitLabel={t('plans.create_submit')} onSubmit={submit} onCancel={onClose} />;
}
