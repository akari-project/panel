// SPDX-License-Identifier: AGPL-3.0-or-later
// 设备与登录会话（spec/32 32.2）：设备列表（含浏览器登录）、最后活跃、移除；设备名额已满时提示（AUTH-14）。
// 名额只统计持有代理凭据的非 web 设备，即 has_credential 为真的设备（web 设备恒为假）。
// 移除设备吊销其会话与代理凭据（AUTH-15）；移除当前设备等同登出，完成后回到登录页。
import { useQuery, useSuspenseQuery } from '@tanstack/react-query';
import { useNavigate, useRouteContext } from '@tanstack/react-router';
import { useId, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { isProblemError, unwrap, type ClientSchemas, type Problem } from '@panel/sdk';
import { Button, ConfirmDialog, EmptyState, ErrorState, LoadingState, ProblemAlert, formatDateTime } from '@panel/ui';
import { devicesQuery, meQuery } from '../queries';

type Device = ClientSchemas['Device'];

export function DevicesPage() {
  const { t } = useTranslation();
  const { api, queryClient } = useRouteContext({ from: '__root__' });
  const navigate = useNavigate();
  const q = useQuery(devicesQuery(api));
  const { data: me } = useSuspenseQuery(meQuery(api));
  const [target, setTarget] = useState<Device | null>(null);
  const [problem, setProblem] = useState<Problem | null>(null);
  const [removed, setRemoved] = useState<string | null>(null);
  const name = useDeviceName();

  const remove = async () => {
    if (!target) return;
    setProblem(null);
    setRemoved(null);
    try {
      await unwrap(api.DELETE('/v1/me/devices/{id}', { params: { path: { id: target.id } } }));
    } catch (e) {
      if (!isProblemError(e)) throw e;
      // 已在别处移除（404）：刷新列表即可，不提示错误。取消重新验证（mfa_required）同样不提示。
      if (e.problem.code !== 'not_found' && e.problem.code !== 'mfa_required') setProblem(e.problem);
      setTarget(null);
      void q.refetch();
      return;
    }
    if (target.is_current) {
      // 服务端已吊销本会话：不再调用登出接口，丢弃本地缓存后回到登录页。
      await navigate({ to: '/login' });
      queryClient.clear();
      return;
    }
    setRemoved(name(target));
    setTarget(null);
    await q.refetch();
  };

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <div className="flex flex-col gap-1">
        <h1 className="text-2xl font-semibold">{t('devices.title')}</h1>
        <p className="text-muted">{t('devices.intro')}</p>
      </div>
      {q.isPending ? (
        <LoadingState />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : (
        <>
          <Usage items={q.data.items} limit={q.data.device_limit} />
          <ProblemAlert problem={problem} />
          <div aria-live="polite" className="text-sm empty:hidden">
            {removed && <p>{t('devices.removed', { name: removed })}</p>}
          </div>
          {q.data.items.length === 0 ? (
            <EmptyState>{t('devices.empty')}</EmptyState>
          ) : (
            <ul aria-label={t('devices.list_label')} className="flex flex-col gap-3">
              {q.data.items.map((d) => (
                <li key={d.id}>
                  <DeviceCard device={d} timeZone={me.timezone} onRemove={() => setTarget(d)} />
                </li>
              ))}
            </ul>
          )}
        </>
      )}
      <ConfirmDialog
        open={target !== null}
        onOpenChange={(o) => !o && setTarget(null)}
        title={target ? t('devices.remove_title', { name: name(target) }) : ''}
        description={
          target?.is_current
            ? t('devices.remove_impact_current')
            : target?.platform === 'web'
              ? t('devices.remove_impact_web')
              : t('devices.remove_impact')
        }
        confirmLabel={t('devices.remove')}
        danger
        onConfirm={remove}
      />
    </div>
  );
}

function useDeviceName() {
  const { t } = useTranslation();
  return (d: Device) => d.model || t(`devices.platforms.${d.platform}`);
}

function Usage({ items, limit }: { items: Device[]; limit: number }) {
  const { t } = useTranslation();
  const used = items.filter((d) => d.has_credential).length;
  const waiting = items.some((d) => d.platform !== 'web' && !d.has_credential);
  const full = used >= limit || waiting;
  return (
    <div className="flex flex-col gap-2">
      <p>
        <span className="font-medium">{t('devices.usage', { used, limit })}</span>
        <span className="ml-2 text-sm text-muted">{t('devices.usage_hint')}</span>
      </p>
      {full && (
        <p role="status" className="rounded-lg border border-danger/50 bg-surface p-4 text-sm">
          {t('devices.limit_reached')}
        </p>
      )}
    </div>
  );
}

function DeviceCard({ device: d, timeZone, onRemove }: { device: Device; timeZone: string; onRemove: () => void }) {
  const { t, i18n } = useTranslation();
  const titleId = useId();
  const name = useDeviceName()(d);
  const lng = i18n.resolvedLanguage ?? 'en';
  const time = (v: string) => formatDateTime(v, lng, timeZone || undefined);
  const facts: [string, string][] = [
    [t('devices.platform'), t(`devices.platforms.${d.platform}`)],
    ...(d.app_version ? [[t('devices.app_version'), d.app_version] as [string, string]] : []),
    [t('devices.last_seen'), d.last_seen_at ? time(d.last_seen_at) : t('devices.never')],
    [t('devices.signed_in'), time(d.created_at)],
    [t('devices.ip_prefix'), d.ip_prefix ?? t('devices.never')],
    [
      t('devices.credential'),
      d.platform === 'web' ? t('devices.credential_web') : d.has_credential ? t('devices.credential_yes') : t('devices.credential_no'),
    ],
  ];
  return (
    <article aria-labelledby={titleId} className="flex flex-col gap-3 rounded-lg border border-border bg-surface p-4 sm:flex-row sm:items-start sm:justify-between">
      <div className="flex min-w-0 flex-col gap-2">
        <h2 id={titleId} className="flex flex-wrap items-center gap-2 font-semibold break-words">
          {name}
          {d.is_current && (
            <span className="rounded-full border border-primary px-2 py-0.5 text-xs font-medium text-primary">{t('devices.current')}</span>
          )}
        </h2>
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
          {facts.map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-muted">{k}</dt>
              <dd className="break-words">{v}</dd>
            </div>
          ))}
        </dl>
      </div>
      <div className="shrink-0">
        <Button variant="secondary" className="w-full sm:w-auto" aria-label={t('devices.remove_named', { name })} onClick={onRemove}>
          {t('devices.remove')}
        </Button>
      </div>
    </article>
  );
}
