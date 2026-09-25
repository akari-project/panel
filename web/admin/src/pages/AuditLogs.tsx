// SPDX-License-Identifier: AGPL-3.0-or-later
// 审计日志查询（spec/10 AUTH-18，只读）。筛选条件保存在地址栏，便于分享与刷新后保留。
// 动作名称按 spec/31 CON-09 的目录本地化，未知动作显示原始标识。导出在 M1-02b 提供。
import { zodResolver } from '@hookform/resolvers/zod';
import { useNavigate, useRouteContext, useSearch } from '@tanstack/react-router';
import { useId, useState } from 'react';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { unwrap, type ConsoleSchemas } from '@panel/sdk';
import { Button, Modal, TextField, formatDateTime } from '@panel/ui';
import { CursorListView, PageHeader, TableFrame, td, th, useCursorList } from '../components/List';
import { useLabels } from '../labels';
import { auditLogsKey } from '../queries';

type AuditLog = ConsoleSchemas['AuditLog'];

/** spec/31 CON-09 的审计动作目录。 */
export const auditActions = [
  'session.create',
  'session.delete',
  'step_up.create',
  'staff.create',
  'staff.update',
  'staff.delete',
  'staff_invitation.create',
  'staff_invitation.revoke',
  'role.create',
  'role.update',
  'role.delete',
] as const;

const filterKeys = ['actor_id', 'action', 'target_type', 'target_id', 'created_from', 'created_to'] as const;
export type AuditSearch = Partial<Record<(typeof filterKeys)[number], string>>;

export function validateAuditSearch(s: Record<string, unknown>): AuditSearch {
  const out: AuditSearch = {};
  for (const k of filterKeys) {
    const v = s[k];
    if (typeof v === 'string' && v.trim()) out[k] = v.trim();
  }
  return out;
}

/** datetime-local 的本地时刻 ↔ RFC 3339（CONV-03）。 */
function toRfc3339(local: string): string | undefined {
  if (!local) return undefined;
  const d = new Date(local);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
}

function toLocalInput(rfc: string | undefined): string {
  if (!rfc) return '';
  const d = new Date(rfc);
  if (Number.isNaN(d.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

const filterSchema = z.object({
  actor_id: z.union([z.literal(''), z.uuid({ error: 'validation.uuid' })]),
  action: z.string().max(100),
  target_type: z.string().max(100),
  target_id: z.string().max(200),
  created_from: z.string(),
  created_to: z.string(),
});
type FilterValues = z.infer<typeof filterSchema>;

export function AuditLogsPage() {
  const { t, i18n } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const search = useSearch({ from: '/app/audit-logs' });
  const navigate = useNavigate({ from: '/audit-logs' });
  const [detail, setDetail] = useState<AuditLog | null>(null);

  const list = useCursorList<AuditLog>([...auditLogsKey, search], (cursor) =>
    unwrap(api.GET('/v1/audit-logs', { params: { query: { ...search, ...(cursor ? { cursor } : {}) } } })),
  );

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title={t('audit.title')}>
        <p className="text-sm text-muted">{t('audit.intro')}</p>
      </PageHeader>
      <AuditFilters
        key={JSON.stringify(search)}
        search={search}
        onApply={(next) => void navigate({ search: next })}
      />
      <CursorListView list={list} empty={t('audit.empty')}>
        {(items) => (
          <TableFrame label={t('audit.title')}>
            <thead>
              <tr>
                <th className={th}>{t('audit.time')}</th>
                <th className={th}>{t('audit.actor')}</th>
                <th className={th}>{t('audit.action_label')}</th>
                <th className={th}>{t('audit.target')}</th>
                <th className={th}>{t('list.actions')}</th>
              </tr>
            </thead>
            <tbody>
              {items.map((log) => (
                <tr key={log.id}>
                  <td className={td}>{formatDateTime(log.created_at, i18n.language)}</td>
                  <td className={td}>{log.actor_email ?? log.actor_id ?? t('audit.system')}</td>
                  <td className={td}>
                    <ActionLabel action={log.action} />
                  </td>
                  <td className={td}>
                    <span className="font-mono text-xs break-all">
                      {log.target_type}
                      {log.target_id ? ` ${log.target_id}` : ''}
                    </span>
                  </td>
                  <td className={td}>
                    <Button variant="ghost" aria-label={t('audit.detail_for', { id: log.id })} onClick={() => setDetail(log)}>
                      {t('audit.detail')}
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </TableFrame>
        )}
      </CursorListView>
      <Modal open={!!detail} onOpenChange={(o) => !o && setDetail(null)} title={t('audit.detail_title')}>
        {detail && <AuditDetail log={detail} onClose={() => setDetail(null)} />}
      </Modal>
    </div>
  );
}

function AuditFilters({ search, onApply }: { search: AuditSearch; onApply: (s: AuditSearch) => void }) {
  const { t } = useTranslation();
  const tc = useTranslation('common').t;
  const labels = useLabels();
  const actionsListId = useId();
  const msg = (k: string | undefined) => (k ? (k === 'validation.uuid' ? t('audit.invalid_uuid') : tc(k)) : undefined);
  const { register, handleSubmit, reset, formState } = useForm<FilterValues>({
    resolver: zodResolver(filterSchema),
    defaultValues: {
      actor_id: search.actor_id ?? '',
      action: search.action ?? '',
      target_type: search.target_type ?? '',
      target_id: search.target_id ?? '',
      created_from: toLocalInput(search.created_from),
      created_to: toLocalInput(search.created_to),
    },
  });
  const submit = handleSubmit((v) => {
    onApply(
      validateAuditSearch({
        ...v,
        created_from: toRfc3339(v.created_from),
        created_to: toRfc3339(v.created_to),
      }),
    );
  });
  return (
    <form noValidate onSubmit={submit} aria-label={t('audit.filters')} className="flex flex-col gap-4 rounded-lg border border-border p-4">
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <TextField
          label={t('audit.action_label')}
          list={actionsListId}
          autoComplete="off"
          error={msg(formState.errors.action?.message)}
          {...register('action')}
        />
        <datalist id={actionsListId}>
          {auditActions.map((a) => (
            <option key={a} value={a}>
              {labels.action(a)}
            </option>
          ))}
        </datalist>
        <TextField
          label={t('audit.actor_id')}
          autoComplete="off"
          spellCheck={false}
          error={msg(formState.errors.actor_id?.message)}
          {...register('actor_id')}
        />
        <TextField label={t('audit.target_type')} autoComplete="off" {...register('target_type')} />
        <TextField label={t('audit.target_id')} autoComplete="off" spellCheck={false} {...register('target_id')} />
        <TextField label={t('audit.created_from')} type="datetime-local" {...register('created_from')} />
        <TextField label={t('audit.created_to')} type="datetime-local" {...register('created_to')} />
      </div>
      <div className="flex flex-wrap gap-2">
        <Button type="submit">{t('audit.apply')}</Button>
        <Button
          variant="secondary"
          onClick={() => {
            reset({ actor_id: '', action: '', target_type: '', target_id: '', created_from: '', created_to: '' });
            onApply({});
          }}
        >
          {t('audit.clear')}
        </Button>
      </div>
    </form>
  );
}

/** 已知动作显示本地化名称与原始标识，未知动作只显示原始标识。 */
function ActionLabel({ action }: { action: string }) {
  const labels = useLabels();
  const label = labels.action(action);
  return label === action ? (
    <span className="font-mono text-xs">{action}</span>
  ) : (
    <>
      <div>{label}</div>
      <div className="font-mono text-xs text-muted">{action}</div>
    </>
  );
}

function AuditDetail({ log, onClose }: { log: AuditLog; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const tc = useTranslation('common').t;
  const labels = useLabels();
  const rows: [string, string][] = [
    [t('audit.time'), formatDateTime(log.created_at, i18n.language)],
    [t('audit.actor'), log.actor_email ?? log.actor_id ?? t('audit.system')],
    [t('audit.action_label'), labels.action(log.action) === log.action ? log.action : `${labels.action(log.action)} (${log.action})`],
    [t('audit.target'), `${log.target_type}${log.target_id ? ` ${log.target_id}` : ''}`],
    [t('audit.reason'), log.reason ?? '—'],
    [t('audit.ip_prefix'), log.ip_prefix ?? '—'],
    [t('audit.request_id'), log.request_id],
  ];
  return (
    <div className="flex flex-col gap-4 text-sm">
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-2">
        {rows.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-muted">{k}</dt>
            <dd className="break-all">{v}</dd>
          </div>
        ))}
      </dl>
      <div className="flex flex-col gap-1">
        <h3 className="font-medium">{t('audit.diff')}</h3>
        {log.diff ? (
          <pre className="max-h-80 overflow-auto rounded-md border border-border bg-surface p-3 font-mono text-xs whitespace-pre-wrap break-all">
            {JSON.stringify(log.diff, null, 2)}
          </pre>
        ) : (
          <p className="text-muted">—</p>
        )}
      </div>
      <div className="flex justify-end">
        <Button variant="secondary" onClick={onClose}>
          {tc('close')}
        </Button>
      </div>
    </div>
  );
}
