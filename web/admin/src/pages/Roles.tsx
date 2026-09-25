// SPDX-License-Identifier: AGPL-3.0-or-later
// 角色管理（spec/10 AUTH-22）：三个内置角色只读；自定义角色的权限取自权限目录（AUTH-17），
// 不能包含 `*` 与 `staff.*`（管理员与角色只由 superadmin 管理）。
// 创建、修改、删除都是敏感操作（AUTH-19）；修改与删除携带 If-Match（CONV-28），版本不一致时提示刷新。
import { zodResolver } from '@hookform/resolvers/zod';
import { useQuery, useQueryClient, useSuspenseQuery } from '@tanstack/react-query';
import { useRouteContext } from '@tanstack/react-router';
import { useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import {
  ProblemError,
  canOperate,
  toProblem,
  unwrap,
  type ConsoleApi,
  type ConsoleSchemas,
  type Problem,
} from '@panel/sdk';
import { Button, EmptyState, ErrorState, LoadingState, Modal, ProblemAlert, TextField, applyFieldErrors } from '@panel/ui';
import { CheckboxGroup, PageHeader, TableFrame, td, th } from '../components/List';
import { useLabels } from '../labels';
import { rolesQuery, staffMeQuery } from '../queries';
import {
  ReasonField,
  SensitiveConfirm,
  auditReasonHeader,
  handleSensitiveError,
  useIdempotencyKey,
  reasonSchema,
  useSensitive,
} from '../sensitive';

type Role = ConsoleSchemas['Role'];
type Permission = ConsoleSchemas['Permission'];
type Grantable = Exclude<Permission, '*' | 'staff.*'>;

/** 可授予自定义角色的权限，按 AUTH-17 权限目录的顺序。 */
export const grantablePermissions = [
  'accounts.read',
  'accounts.adjust',
  'credits.adjust',
  'orders.read',
  'orders.refund',
  'plans.*',
  'location-groups.*',
  'hosts.*',
  'kernels.write',
  'coupons.*',
  'content.*',
  'tickets.*',
  'payments.configure',
  'settings.read',
  'settings.write',
  'audit.read',
] as const satisfies readonly Grantable[];

// 权限目录新增取值时，这里编译失败，提醒补充上面的列表。
type Missing = Exclude<Grantable, (typeof grantablePermissions)[number]>;
const exhaustive: [Missing] extends [never] ? true : Missing = true;
void exhaustive;

/** 读取单个角色及其 ETag，用于 If-Match（CONV-28）。 */
async function getRoleWithEtag(api: ConsoleApi, name: string): Promise<{ role: Role; etag: string }> {
  const { data, error, response } = await api.GET('/v1/roles/{id}', { params: { path: { id: name } } });
  if (!response.ok || !data) throw new ProblemError(toProblem(error, response));
  return { role: data, etag: response.headers.get('ETag') ?? '' };
}

export function RolesPage() {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const { data: me } = useSuspenseQuery(staffMeQuery(api));
  const labels = useLabels();
  const roles = useQuery(rolesQuery(api));
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<Role | null>(null);
  const [deleting, setDeleting] = useState<Role | null>(null);
  const canCreate = canOperate(me, 'POST /v1/roles');
  const canEdit = canOperate(me, 'PATCH /v1/roles/{id}');
  const canDelete = canOperate(me, 'DELETE /v1/roles/{id}');

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={t('roles.title')}
        actions={canCreate && <Button onClick={() => setCreating(true)}>{t('roles.create')}</Button>}
      >
        <p className="text-sm text-muted">{t('roles.intro')}</p>
      </PageHeader>
      {roles.isPending ? (
        <LoadingState />
      ) : roles.isError ? (
        <ErrorState error={roles.error} onRetry={() => void roles.refetch()} />
      ) : roles.data.length === 0 ? (
        <EmptyState />
      ) : (
        <TableFrame label={t('roles.title')}>
          <thead>
            <tr>
              <th className={th}>{t('roles.name')}</th>
              <th className={th}>{t('roles.permissions')}</th>
              <th className={th}>{t('roles.staff_count')}</th>
              {(canEdit || canDelete) && <th className={th}>{t('list.actions')}</th>}
            </tr>
          </thead>
          <tbody>
            {roles.data.map((r) => (
              <tr key={r.name}>
                <td className={td}>
                  <div className="font-medium">{labels.role(r.name)}</div>
                  {r.description && <div className="text-muted">{r.description}</div>}
                  {r.is_builtin && <div className="text-muted">{t('roles.builtin_badge')}</div>}
                </td>
                <td className={td}>
                  {r.permissions.includes('*') ? t('roles.all_permissions') : r.permissions.map(labels.permission).join('、')}
                </td>
                <td className={td}>{r.staff_count}</td>
                {(canEdit || canDelete) && (
                  <td className={td}>
                    {!r.is_builtin && (
                      <div className="flex flex-wrap gap-1">
                        {canEdit && (
                          <Button variant="ghost" aria-label={t('roles.edit_for', { name: r.name })} onClick={() => setEditing(r)}>
                            {t('roles.edit')}
                          </Button>
                        )}
                        {canDelete && (
                          <Button variant="ghost" aria-label={t('roles.delete_for', { name: r.name })} onClick={() => setDeleting(r)}>
                            {t('roles.delete')}
                          </Button>
                        )}
                      </div>
                    )}
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </TableFrame>
      )}
      <RoleDialog open={creating} onClose={() => setCreating(false)} />
      <RoleDialog open={!!editing} role={editing ?? undefined} onClose={() => setEditing(null)} />
      <DeleteRoleDialog role={deleting} onClose={() => setDeleting(null)} />
    </div>
  );
}

const nameSchema = z.string().regex(/^[a-z][a-z0-9-]{1,31}$/, { error: 'roles.name_format' });
const roleSchema = z.object({
  name: nameSchema,
  description: z.string().max(200),
  permissions: z.array(z.string()).min(1, { error: 'validation.required' }),
  reason: reasonSchema,
});
type RoleValues = z.infer<typeof roleSchema>;

function RoleDialog({ open, role, onClose }: { open: boolean; role?: Role; onClose: () => void }) {
  const { t } = useTranslation();
  return (
    <Modal
      open={open}
      onOpenChange={(o) => !o && onClose()}
      title={role ? t('roles.edit_title', { name: role.name }) : t('roles.create')}
      description={t('roles.form_description')}
    >
      {open && <RoleForm role={role} onClose={onClose} />}
    </Modal>
  );
}

function RoleForm({ role, onClose }: { role?: Role | undefined; onClose: () => void }) {
  const { t } = useTranslation();
  const tc = useTranslation('common').t;
  const { api } = useRouteContext({ from: '__root__' });
  const queryClient = useQueryClient();
  const labels = useLabels();
  const sensitive = useSensitive();
  const [problem, setProblem] = useState<Problem | null>(null);
  const idempotencyKey = useIdempotencyKey();
  // 翻译键可能来自 admin（roles.name_format）或 common（validation.*、field_errors.*）命名空间。
  const msg = (k: string | undefined) => (k ? (k.startsWith('roles.') ? t(k) : tc(k)) : undefined);
  const { register, control, handleSubmit, setError, formState } = useForm<RoleValues>({
    resolver: zodResolver(roleSchema),
    defaultValues: {
      name: role?.name ?? '',
      description: role?.description ?? '',
      permissions: role ? role.permissions.filter((p) => (grantablePermissions as readonly string[]).includes(p)) : [],
      reason: '',
    },
  });
  const fields = { name: 'name', description: 'description', permissions: 'permissions', reason: 'reason' } as const;

  const submit = handleSubmit(async (v) => {
    setProblem(null);
    const permissions = v.permissions as Grantable[];
    const description = v.description.trim() || null;
    try {
      await sensitive(async (assertion) => {
        if (role) {
          // 以最新的 ETag 修改；期间被他人修改时返回 409 conflict（CONV-28）。
          const { etag } = await getRoleWithEtag(api, role.name);
          await unwrap(
            api.PATCH('/v1/roles/{id}', {
              params: { path: { id: role.name }, header: { 'Mfa-Assertion': assertion, 'If-Match': etag } },
              body: { description, permissions, reason: v.reason.trim() },
            }),
          );
        } else {
          const body = { name: v.name, description, permissions, reason: v.reason.trim() };
          await unwrap(
            api.POST('/v1/roles', {
              params: { header: { 'Mfa-Assertion': assertion, 'Idempotency-Key': idempotencyKey(body) } },
              body,
            }),
          );
        }
      });
    } catch (e) {
      setProblem(handleSensitiveError(e, (p) => applyFieldErrors(p, fields, setError, tc)));
      return;
    }
    await queryClient.invalidateQueries({ queryKey: ['roles'] });
    onClose();
  });

  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      {/* 名称是角色的标识，创建后不可修改。 */}
      {!role && (
        <TextField
          label={t('roles.name')}
          hint={t('roles.name_hint')}
          autoComplete="off"
          error={msg(formState.errors.name?.message)}
          {...register('name')}
        />
      )}
      <TextField
        label={t('roles.description')}
        autoComplete="off"
        maxLength={200}
        error={msg(formState.errors.description?.message)}
        {...register('description')}
      />
      <Controller
        control={control}
        name="permissions"
        render={({ field, fieldState }) => (
          <CheckboxGroup
            legend={t('roles.permissions')}
            options={grantablePermissions.map((p) => ({ value: p, label: labels.permission(p), description: p }))}
            value={field.value}
            onChange={field.onChange}
            error={msg(fieldState.error?.message)}
          />
        )}
      />
      {role && role.staff_count > 0 && (
        <p className="rounded-md border border-border bg-surface p-3 text-sm">
          {t('roles.edit_impact', { count: role.staff_count })}
        </p>
      )}
      <ReasonField error={msg(formState.errors.reason?.message)} {...register('reason')} />
      <ProblemAlert problem={problem} />
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        <Button variant="secondary" onClick={onClose} disabled={formState.isSubmitting}>
          {tc('cancel')}
        </Button>
        <Button type="submit" loading={formState.isSubmitting}>
          {role ? t('roles.save') : t('roles.create_submit')}
        </Button>
      </div>
    </form>
  );
}

function DeleteRoleDialog({ role, onClose }: { role: Role | null; onClose: () => void }) {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const queryClient = useQueryClient();
  const sensitive = useSensitive();
  return (
    <SensitiveConfirm
      open={!!role}
      onOpenChange={(o) => !o && onClose()}
      title={t('roles.delete_title', { name: role?.name ?? '' })}
      description={role ? t('roles.delete_description', { count: role.staff_count }) : ''}
      confirmLabel={t('roles.delete')}
      onConfirm={async (reason) => {
        if (!role) return;
        await sensitive(async (assertion) => {
          const { etag } = await getRoleWithEtag(api, role.name);
          await unwrap(
            api.DELETE('/v1/roles/{id}', {
              params: {
                path: { id: role.name },
                header: { 'Mfa-Assertion': assertion, 'Audit-Reason': auditReasonHeader(reason), 'If-Match': etag },
              },
            }),
          );
        });
        await queryClient.invalidateQueries({ queryKey: ['roles'] });
      }}
    />
  );
}
