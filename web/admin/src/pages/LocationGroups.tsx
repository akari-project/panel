// SPDX-License-Identifier: AGPL-3.0-or-later
// 线路组（spec/32 32.3“套餐与价格、线路组”）：列表、新建、编辑、删除。成员（节点）管理在 M2。
// 修改 min_tier 前调用影响预览并确认（UI-03、ACS-05）；修改与删除携带 If-Match（CONV-28）；
// 仍被套餐引用的线路组不能删除（409 invalid_state，ACS-06）。
import { zodResolver } from '@hookform/resolvers/zod';
import { useQuery, useQueryClient, useSuspenseQuery } from '@tanstack/react-query';
import { useRouteContext } from '@tanstack/react-router';
import { useState } from 'react';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { canOperate, isProblemError, unwrap, type ConsoleApi, type Problem } from '@panel/sdk';
import { Button, ErrorState, LoadingState, Modal, ProblemAlert, TextAreaField, TextField, applyFieldErrors } from '@panel/ui';
import { ActionConfirm, useAsk } from '../components/Confirm';
import { CursorListView, PageHeader, TableFrame, td, th, useCursorList } from '../components/List';
import { isConflict, locationGroupsKey, unwrapWithEtag, useFormatters, type LocationGroup } from '../plans';
import { staffMeQuery } from '../queries';
import { useIdempotencyKey } from '../sensitive';
import { useImpactText } from './PlanDetail';

const groupQuery = (api: ConsoleApi, id: string) => ({
  queryKey: [...locationGroupsKey, 'detail', id],
  queryFn: () => unwrapWithEtag(api.GET('/v1/location-groups/{id}', { params: { path: { id } } })),
});

export function LocationGroupsPage() {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const { data: me } = useSuspenseQuery(staffMeQuery(api));
  const f = useFormatters();
  const list = useCursorList<LocationGroup>([...locationGroupsKey, 'list'], (cursor) =>
    unwrap(api.GET('/v1/location-groups', { params: { query: cursor ? { cursor } : {} } })),
  );
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<LocationGroup | null>(null);
  const [deleting, setDeleting] = useState<LocationGroup | null>(null);
  const canCreate = canOperate(me, 'POST /v1/location-groups');
  const canEdit = canOperate(me, 'PATCH /v1/location-groups/{id}');
  const canDelete = canOperate(me, 'DELETE /v1/location-groups/{id}');

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title={t('groups.title')} actions={canCreate && <Button onClick={() => setCreating(true)}>{t('groups.create')}</Button>}>
        <p className="text-sm text-muted">{t('groups.intro')}</p>
      </PageHeader>
      <CursorListView list={list} empty={t('groups.empty')}>
        {(items) => (
          <TableFrame label={t('groups.title')}>
            <thead>
              <tr>
                <th className={th}>{t('groups.name')}</th>
                <th className={th}>{t('groups.min_tier')}</th>
                <th className={th}>{t('groups.host_count')}</th>
                <th className={th}>{t('groups.plan_count')}</th>
                {(canEdit || canDelete) && <th className={th}>{t('list.actions')}</th>}
              </tr>
            </thead>
            <tbody>
              {items.map((g) => (
                <tr key={g.id}>
                  <td className={td}>
                    <div className="font-medium">{g.name}</div>
                    {g.description && <div className="text-muted">{g.description}</div>}
                  </td>
                  <td className={td}>{f.minTier(g.min_tier)}</td>
                  <td className={td}>{g.host_count}</td>
                  <td className={td}>{g.plan_ids.length}</td>
                  {(canEdit || canDelete) && (
                    <td className={td}>
                      <div className="flex flex-wrap gap-1">
                        {canEdit && (
                          <Button variant="ghost" aria-label={t('groups.edit_for', { name: g.name })} onClick={() => setEditing(g)}>
                            {t('groups.edit')}
                          </Button>
                        )}
                        {canDelete && (
                          <Button variant="ghost" aria-label={t('groups.delete_for', { name: g.name })} onClick={() => setDeleting(g)}>
                            {t('groups.delete')}
                          </Button>
                        )}
                      </div>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </TableFrame>
        )}
      </CursorListView>
      <Modal open={creating} onOpenChange={setCreating} title={t('groups.create')} description={t('groups.form_description')}>
        {creating && <GroupForm onClose={() => setCreating(false)} />}
      </Modal>
      <Modal
        open={!!editing}
        onOpenChange={(o) => !o && setEditing(null)}
        title={t('groups.edit_title', { name: editing?.name ?? '' })}
        description={t('groups.form_description')}
      >
        {editing && <EditGroup id={editing.id} onClose={() => setEditing(null)} />}
      </Modal>
      <DeleteGroupDialog group={deleting} onClose={() => setDeleting(null)} />
    </div>
  );
}

/** 编辑前读取线路组与 ETag（列表不带 ETag），表单以最新数据填充。 */
function EditGroup({ id, onClose }: { id: string; onClose: () => void }) {
  const { api } = useRouteContext({ from: '__root__' });
  const q = useQuery(groupQuery(api, id));
  if (q.isPending) return <LoadingState />;
  if (q.isError) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  return <GroupForm key={q.data.etag} group={q.data.data} etag={q.data.etag} onReload={() => void q.refetch()} onClose={onClose} />;
}

const schema = z.object({
  name: z.string().trim().min(1, { error: 'validation.required' }).max(100, { error: 'field_errors.too_long' }),
  description: z.string().max(2000, { error: 'field_errors.too_long' }),
  min_tier: z.union([
    z.literal(''),
    z
      .string()
      .trim()
      .regex(/^\d+$/, { error: 'plans.validation.integer' })
      .refine((v) => Number(v) <= 2 ** 31 - 1, { error: 'field_errors.out_of_range' }),
  ]),
});
type GroupValues = z.infer<typeof schema>;

function GroupForm({
  group,
  etag,
  onReload,
  onClose,
}: {
  group?: LocationGroup;
  etag?: string;
  onReload?: () => void;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const queryClient = useQueryClient();
  const impactText = useImpactText();
  const idempotencyKey = useIdempotencyKey();
  const [askElement, ask] = useAsk();
  const [problem, setProblem] = useState<Problem | null>(null);
  const { register, handleSubmit, setError, formState } = useForm<GroupValues>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: group?.name ?? '',
      description: group?.description ?? '',
      min_tier: group?.min_tier === null || group?.min_tier === undefined ? '' : String(group.min_tier),
    },
  });
  const err = (k: keyof GroupValues) => {
    const m = formState.errors[k]?.message;
    return m ? t(m) : undefined;
  };

  const submit = handleSubmit(async (v) => {
    setProblem(null);
    const fields = { name: v.name.trim(), description: v.description.trim() || null, min_tier: v.min_tier === '' ? null : Number(v.min_tier) };
    try {
      if (group) {
        const patch: Partial<typeof fields> = {};
        for (const k of Object.keys(fields) as (keyof typeof fields)[]) {
          if ((group[k] ?? null) !== fields[k]) (patch as Record<string, unknown>)[k] = fields[k];
        }
        if (Object.keys(patch).length === 0) {
          onClose();
          return;
        }
        // 最低等级变化会改变哪些套餐的用户能使用该组（ACS-05）：先预览影响再确认。
        if ('min_tier' in patch) {
          const impact = await unwrap(
            api.POST('/v1/location-groups/{id}/impact', { params: { path: { id: group.id } }, body: { min_tier: patch.min_tier ?? null } }),
          );
          const ok = await ask({ title: t('groups.confirm_title'), description: impactText(impact), confirmLabel: t('groups.confirm') });
          if (!ok) return;
        }
        await unwrap(api.PATCH('/v1/location-groups/{id}', { params: { path: { id: group.id }, header: { 'If-Match': etag ?? '' } }, body: patch }));
      } else {
        await unwrap(api.POST('/v1/location-groups', { params: { header: { 'Idempotency-Key': idempotencyKey(fields) } }, body: fields }));
      }
    } catch (e) {
      if (!isProblemError(e)) throw e;
      setProblem(applyFieldErrors(e.problem, { name: 'name', description: 'description', min_tier: 'min_tier' }, setError, t));
      return;
    }
    await queryClient.invalidateQueries({ queryKey: locationGroupsKey });
    onClose();
  });

  return (
    <>
      <form noValidate onSubmit={submit} className="flex flex-col gap-4">
        <TextField label={t('groups.name')} autoComplete="off" maxLength={100} error={err('name')} {...register('name')} />
        <TextAreaField label={t('groups.description')} error={err('description')} {...register('description')} />
        <TextField label={t('groups.min_tier')} inputMode="numeric" hint={t('groups.min_tier_hint')} error={err('min_tier')} {...register('min_tier')} />
        {problem && (
          <div className="flex flex-col gap-2">
            <ProblemAlert problem={problem} message={isConflict(problem) ? t('groups.conflict') : undefined} />
            {isConflict(problem) && onReload && (
              <div>
                <Button variant="secondary" onClick={onReload}>
                  {t('plans.reload')}
                </Button>
              </div>
            )}
          </div>
        )}
        <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
          <Button variant="secondary" onClick={onClose} disabled={formState.isSubmitting}>
            {t('common:cancel')}
          </Button>
          <Button type="submit" loading={formState.isSubmitting}>
            {group ? t('groups.save') : t('groups.create_submit')}
          </Button>
        </div>
      </form>
      {askElement}
    </>
  );
}

function DeleteGroupDialog({ group, onClose }: { group: LocationGroup | null; onClose: () => void }) {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const queryClient = useQueryClient();
  return (
    <ActionConfirm
      open={!!group}
      onOpenChange={(o) => !o && onClose()}
      title={t('groups.delete_title', { name: group?.name ?? '' })}
      description={
        group && group.plan_ids.length > 0 ? t('groups.delete_in_use', { count: group.plan_ids.length }) : t('groups.delete_description')
      }
      confirmLabel={t('groups.delete')}
      danger
      problemMessage={(p) => (p.code === 'invalid_state' ? t('groups.delete_invalid_state') : undefined)}
      onConfirm={async () => {
        if (!group) return;
        const { etag } = await unwrapWithEtag(api.GET('/v1/location-groups/{id}', { params: { path: { id: group.id } } }));
        await unwrap(api.DELETE('/v1/location-groups/{id}', { params: { path: { id: group.id }, header: { 'If-Match': etag } } }));
        await queryClient.invalidateQueries({ queryKey: locationGroupsKey });
      }}
    />
  );
}
