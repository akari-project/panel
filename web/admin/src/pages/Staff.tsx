// SPDX-License-Identifier: AGPL-3.0-or-later
// 管理员与邀请（spec/10 AUTH-22、spec/32 32.3）。新管理员只能通过邀请加入；
// 邀请、修改角色、移除、撤销邀请都是敏感操作（AUTH-19）：原因、Mfa-Assertion、二次确认。
import { zodResolver } from '@hookform/resolvers/zod';
import { useQuery, useQueryClient, useSuspenseQuery } from '@tanstack/react-query';
import { Link, useRouteContext } from '@tanstack/react-router';
import { useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { canOperate, unwrap, type ConsoleSchemas, type Problem } from '@panel/sdk';
import { Button, Modal, ProblemAlert, TextField, applyFieldErrors, formatDateTime, navLinkClass } from '@panel/ui';
import { CheckboxGroup, CursorListView, PageHeader, TableFrame, td, th, useCursorList } from '../components/List';
import { useLabels } from '../labels';
import { invitationsKey, rolesQuery, staffListKey, staffMeQuery } from '../queries';
import {
  ReasonField,
  SensitiveConfirm,
  auditReasonHeader,
  handleSensitiveError,
  useIdempotencyKey,
  reasonSchema,
  useSensitive,
} from '../sensitive';

type Staff = ConsoleSchemas['Staff'];
type Invitation = ConsoleSchemas['StaffInvitation'];

function StaffTabs() {
  const { t } = useTranslation();
  return (
    <nav aria-label={t('staff.tabs_label')}>
      <ul className="flex gap-1 border-b border-border">
        <li>
          <Link to="/staff" className={navLinkClass} activeOptions={{ exact: true }}>
            {t('staff.tab_staff')}
          </Link>
        </li>
        <li>
          <Link to="/staff/invitations" className={navLinkClass}>
            {t('staff.tab_invitations')}
          </Link>
        </li>
      </ul>
    </nav>
  );
}

function RoleList({ roles }: { roles: readonly string[] }) {
  const labels = useLabels();
  return <span>{roles.map(labels.role).join('、')}</span>;
}

function useDateTime() {
  const { i18n } = useTranslation();
  return (v: string | null | undefined) => (v ? formatDateTime(v, i18n.language) : '—');
}

export function StaffPage() {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const { data: me } = useSuspenseQuery(staffMeQuery(api));
  const dt = useDateTime();
  const list = useCursorList<Staff>(staffListKey, (cursor) =>
    unwrap(api.GET('/v1/staff', { params: { query: cursor ? { cursor } : {} } })),
  );
  const [inviting, setInviting] = useState(false);
  const [notice, setNotice] = useState('');
  const [editing, setEditing] = useState<Staff | null>(null);
  const [removing, setRemoving] = useState<Staff | null>(null);
  const canInvite = canOperate(me, 'POST /v1/staff-invitations');
  const canEdit = canOperate(me, 'PATCH /v1/staff/{id}');
  const canRemove = canOperate(me, 'DELETE /v1/staff/{id}');

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={t('staff.title')}
        actions={canInvite && <Button onClick={() => setInviting(true)}>{t('staff.invite')}</Button>}
      />
      <StaffTabs />
      {notice && <Notice text={notice} />}
      <CursorListView list={list}>
        {(items) => (
          <TableFrame label={t('staff.title')}>
            <thead>
              <tr>
                <th className={th}>{t('staff.email')}</th>
                <th className={th}>{t('staff.roles')}</th>
                <th className={th}>{t('staff.totp')}</th>
                <th className={th}>{t('staff.last_login')}</th>
                {(canEdit || canRemove) && <th className={th}>{t('list.actions')}</th>}
              </tr>
            </thead>
            <tbody>
              {items.map((s) => (
                <tr key={s.account_id}>
                  <td className={td}>
                    {s.email}
                    {s.account_id === me.account_id && <span className="ml-2 text-muted">{t('staff.you')}</span>}
                  </td>
                  <td className={td}>
                    <RoleList roles={s.roles} />
                  </td>
                  <td className={td}>{s.has_totp ? t('staff.totp_on') : t('staff.totp_off')}</td>
                  <td className={td}>{dt(s.last_login_at)}</td>
                  {(canEdit || canRemove) && (
                    <td className={td}>
                      <div className="flex flex-wrap gap-1">
                        {canEdit && (
                          <Button variant="ghost" aria-label={t('staff.edit_roles_for', { email: s.email })} onClick={() => setEditing(s)}>
                            {t('staff.edit_roles')}
                          </Button>
                        )}
                        {canRemove && (
                          <Button variant="ghost" aria-label={t('staff.remove_for', { email: s.email })} onClick={() => setRemoving(s)}>
                            {t('staff.remove')}
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
      <InviteDialog open={inviting} onOpenChange={setInviting} onInvited={(email) => setNotice(t('staff.invited', { email }))} />
      <EditRolesDialog staff={editing} onClose={() => setEditing(null)} />
      <RemoveStaffDialog staff={removing} onClose={() => setRemoving(null)} />
    </div>
  );
}

const rolesField = z.array(z.string()).min(1, { error: 'validation.required' });

function RolesCheckboxes({
  value,
  onChange,
  error,
}: {
  value: string[];
  onChange: (v: string[]) => void;
  error?: string | undefined;
}) {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const labels = useLabels();
  const roles = useQuery(rolesQuery(api));
  if (roles.isError) return <ProblemAlert problem={(roles.error as { problem?: Problem }).problem ?? null} />;
  return (
    <CheckboxGroup
      legend={t('staff.roles')}
      options={(roles.data ?? []).map((r) => ({ value: r.name, label: labels.role(r.name), description: r.description ?? undefined }))}
      value={value}
      onChange={onChange}
      error={error}
    />
  );
}

const inviteSchema = z.object({
  email: z.email({ error: 'validation.email' }),
  roles: rolesField,
  reason: reasonSchema,
});
type InviteValues = z.infer<typeof inviteSchema>;

function InviteDialog({
  open,
  onOpenChange,
  onInvited,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onInvited: (email: string) => void;
}) {
  const { t } = useTranslation();
  return (
    <Modal open={open} onOpenChange={onOpenChange} title={t('staff.invite')} description={t('staff.invite_description')}>
      {open && (
        <InviteForm
          onClose={() => onOpenChange(false)}
          onInvited={(email) => {
            onOpenChange(false);
            onInvited(email);
          }}
        />
      )}
    </Modal>
  );
}

/** 操作成功的提示（屏幕阅读器通过 role=status 读出）。 */
function Notice({ text }: { text: string }) {
  return (
    <p role="status" className="rounded-md border border-border bg-surface p-3 text-sm">
      {text}
    </p>
  );
}

function InviteForm({ onClose, onInvited }: { onClose: () => void; onInvited: (email: string) => void }) {
  const { t } = useTranslation();
  const tc = useTranslation('common').t;
  const { api } = useRouteContext({ from: '__root__' });
  const queryClient = useQueryClient();
  const sensitive = useSensitive();
  const [problem, setProblem] = useState<Problem | null>(null);
  // 同一内容的重复提交使用同一个幂等键；请求层在重新验证后的自动重试也沿用它（CONV-12）。
  const idempotencyKey = useIdempotencyKey();
  const { register, control, handleSubmit, setError, formState } = useForm<InviteValues>({
    resolver: zodResolver(inviteSchema),
    defaultValues: { email: '', roles: [], reason: '' },
  });
  const submit = handleSubmit(async (v) => {
    setProblem(null);
    try {
      const body = { email: v.email, roles: v.roles, reason: v.reason.trim() };
      await sensitive((assertion) =>
        unwrap(
          api.POST('/v1/staff-invitations', {
            params: { header: { 'Mfa-Assertion': assertion, 'Idempotency-Key': idempotencyKey(body) } },
            body,
          }),
        ),
      );
    } catch (e) {
      setProblem(
        handleSensitiveError(e, (p) => applyFieldErrors(p, { email: 'email', roles: 'roles', reason: 'reason' }, setError, tc)),
      );
      return;
    }
    await queryClient.invalidateQueries({ queryKey: invitationsKey });
    onInvited(v.email);
  });
  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <TextField
        label={t('staff.email')}
        type="email"
        autoComplete="off"
        error={formState.errors.email?.message && tc(formState.errors.email.message)}
        {...register('email')}
      />
      <Controller
        control={control}
        name="roles"
        render={({ field, fieldState }) => (
          <RolesCheckboxes
            value={field.value}
            onChange={field.onChange}
            error={fieldState.error?.message && tc(fieldState.error.message)}
          />
        )}
      />
      <ReasonField error={formState.errors.reason?.message && tc(formState.errors.reason.message)} {...register('reason')} />
      <ProblemAlert problem={problem} />
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        <Button variant="secondary" onClick={onClose} disabled={formState.isSubmitting}>
          {tc('cancel')}
        </Button>
        <Button type="submit" loading={formState.isSubmitting}>
          {t('staff.invite_submit')}
        </Button>
      </div>
    </form>
  );
}

const editSchema = z.object({ roles: rolesField, reason: reasonSchema });
type EditValues = z.infer<typeof editSchema>;

function EditRolesDialog({ staff, onClose }: { staff: Staff | null; onClose: () => void }) {
  const { t } = useTranslation();
  return (
    <Modal
      open={!!staff}
      onOpenChange={(o) => !o && onClose()}
      title={t('staff.edit_roles')}
      description={staff ? t('staff.edit_roles_description', { email: staff.email }) : undefined}
    >
      {staff && <EditRolesForm staff={staff} onClose={onClose} />}
    </Modal>
  );
}

function EditRolesForm({ staff, onClose }: { staff: Staff; onClose: () => void }) {
  const { t } = useTranslation();
  const tc = useTranslation('common').t;
  const { api } = useRouteContext({ from: '__root__' });
  const queryClient = useQueryClient();
  const sensitive = useSensitive();
  const [problem, setProblem] = useState<Problem | null>(null);
  const { register, control, handleSubmit, setError, formState } = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: { roles: [...staff.roles], reason: '' },
  });
  const submit = handleSubmit(async (v) => {
    setProblem(null);
    try {
      await sensitive((assertion) =>
        unwrap(
          api.PATCH('/v1/staff/{id}', {
            params: { path: { id: staff.account_id }, header: { 'Mfa-Assertion': assertion } },
            body: { roles: v.roles, reason: v.reason.trim() },
          }),
        ),
      );
    } catch (e) {
      setProblem(handleSensitiveError(e, (p) => applyFieldErrors(p, { roles: 'roles', reason: 'reason' }, setError, tc)));
      return;
    }
    // 角色变化会吊销该管理员的全部管理会话（AUTH-21）；修改的是自己时，下一次请求将回到登录页。
    await queryClient.invalidateQueries({ queryKey: ['staff'] });
    await queryClient.invalidateQueries({ queryKey: ['roles'] });
    onClose();
  });
  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <p className="rounded-md border border-border bg-surface p-3 text-sm">{t('staff.sessions_revoked_warning')}</p>
      <Controller
        control={control}
        name="roles"
        render={({ field, fieldState }) => (
          <RolesCheckboxes
            value={field.value}
            onChange={field.onChange}
            error={fieldState.error?.message && tc(fieldState.error.message)}
          />
        )}
      />
      <ReasonField error={formState.errors.reason?.message && tc(formState.errors.reason.message)} {...register('reason')} />
      <ProblemAlert problem={problem} />
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        <Button variant="secondary" onClick={onClose} disabled={formState.isSubmitting}>
          {tc('cancel')}
        </Button>
        <Button type="submit" loading={formState.isSubmitting}>
          {t('staff.edit_roles_submit')}
        </Button>
      </div>
    </form>
  );
}

function RemoveStaffDialog({ staff, onClose }: { staff: Staff | null; onClose: () => void }) {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const queryClient = useQueryClient();
  const sensitive = useSensitive();
  return (
    <SensitiveConfirm
      open={!!staff}
      onOpenChange={(o) => !o && onClose()}
      title={t('staff.remove_title')}
      description={staff ? t('staff.remove_description', { email: staff.email }) : ''}
      confirmLabel={t('staff.remove')}
      onConfirm={async (reason) => {
        if (!staff) return;
        await sensitive((assertion) =>
          unwrap(
            api.DELETE('/v1/staff/{id}', {
              params: { path: { id: staff.account_id }, header: { 'Mfa-Assertion': assertion, 'Audit-Reason': auditReasonHeader(reason) } },
            }),
          ),
        );
        await queryClient.invalidateQueries({ queryKey: ['staff'] });
        await queryClient.invalidateQueries({ queryKey: ['roles'] });
      }}
    />
  );
}

const invitationStatuses = ['pending', 'accepted', 'expired', 'revoked'] as const;
type InvitationStatus = (typeof invitationStatuses)[number];

export function InvitationsPage() {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const { data: me } = useSuspenseQuery(staffMeQuery(api));
  const dt = useDateTime();
  const [status, setStatus] = useState<InvitationStatus | ''>('pending');
  const list = useCursorList<Invitation>([...invitationsKey, status], (cursor) =>
    unwrap(
      api.GET('/v1/staff-invitations', {
        params: { query: { ...(status ? { status } : {}), ...(cursor ? { cursor } : {}) } },
      }),
    ),
  );
  const [inviting, setInviting] = useState(false);
  const [notice, setNotice] = useState('');
  const [revoking, setRevoking] = useState<Invitation | null>(null);
  const canInvite = canOperate(me, 'POST /v1/staff-invitations');
  const canRevoke = canOperate(me, 'DELETE /v1/staff-invitations/{id}');

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={t('staff.title')}
        actions={canInvite && <Button onClick={() => setInviting(true)}>{t('staff.invite')}</Button>}
      />
      <StaffTabs />
      {notice && <Notice text={notice} />}
      <div className="flex max-w-xs flex-col gap-1">
        <label htmlFor="invitation-status" className="text-sm font-medium">
          {t('invitations.status')}
        </label>
        <select
          id="invitation-status"
          className="min-h-10 rounded-md border border-border bg-bg px-3 text-base text-fg"
          value={status}
          onChange={(e) => setStatus(e.target.value as InvitationStatus | '')}
        >
          <option value="">{t('invitations.status_all')}</option>
          {invitationStatuses.map((s) => (
            <option key={s} value={s}>
              {t(`invitations.statuses.${s}`)}
            </option>
          ))}
        </select>
      </div>
      <CursorListView list={list}>
        {(items) => (
          <TableFrame label={t('staff.tab_invitations')}>
            <thead>
              <tr>
                <th className={th}>{t('staff.email')}</th>
                <th className={th}>{t('staff.roles')}</th>
                <th className={th}>{t('invitations.status')}</th>
                <th className={th}>{t('invitations.expires_at')}</th>
                {canRevoke && <th className={th}>{t('list.actions')}</th>}
              </tr>
            </thead>
            <tbody>
              {items.map((inv) => (
                <tr key={inv.id}>
                  <td className={td}>{inv.email}</td>
                  <td className={td}>
                    <RoleList roles={inv.roles} />
                  </td>
                  <td className={td}>{t(`invitations.statuses.${inv.status}`)}</td>
                  <td className={td}>{dt(inv.expires_at)}</td>
                  {canRevoke && (
                    <td className={td}>
                      {inv.status === 'pending' && (
                        <Button
                          variant="ghost"
                          aria-label={t('invitations.revoke_for', { email: inv.email })}
                          onClick={() => setRevoking(inv)}
                        >
                          {t('invitations.revoke')}
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
      <InviteDialog open={inviting} onOpenChange={setInviting} onInvited={(email) => setNotice(t('staff.invited', { email }))} />
      <RevokeInvitationDialog invitation={revoking} onClose={() => setRevoking(null)} />
    </div>
  );
}

function RevokeInvitationDialog({ invitation, onClose }: { invitation: Invitation | null; onClose: () => void }) {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const queryClient = useQueryClient();
  const sensitive = useSensitive();
  return (
    <SensitiveConfirm
      open={!!invitation}
      onOpenChange={(o) => !o && onClose()}
      title={t('invitations.revoke_title')}
      description={invitation ? t('invitations.revoke_description', { email: invitation.email }) : ''}
      confirmLabel={t('invitations.revoke')}
      onConfirm={async (reason) => {
        if (!invitation) return;
        await sensitive((assertion) =>
          unwrap(
            api.DELETE('/v1/staff-invitations/{id}', {
              params: {
                path: { id: invitation.id },
                header: { 'Mfa-Assertion': assertion, 'Audit-Reason': auditReasonHeader(reason) },
              },
            }),
          ),
        );
        await queryClient.invalidateQueries({ queryKey: invitationsKey });
      }}
    />
  );
}
