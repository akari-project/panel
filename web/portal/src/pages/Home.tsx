// SPDX-License-Identifier: AGPL-3.0-or-later
// 概览（M1 起填充内容）。当前只显示欢迎语与空状态。
import { useSuspenseQuery } from '@tanstack/react-query';
import { Link, useRouteContext } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { EmptyState } from '@panel/ui';
import { meQuery } from '../queries';

export function HomePage() {
  const { t } = useTranslation();
  const { api } = useRouteContext({ from: '__root__' });
  const { data: me } = useSuspenseQuery(meQuery(api));
  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">{t('home.title')}</h1>
        <p className="text-muted">{t('home.welcome', { email: me.email })}</p>
      </div>
      {!me.is_email_verified && (
        <div className="flex flex-col gap-2 rounded-lg border border-border bg-surface p-4 sm:flex-row sm:items-center sm:justify-between">
          <p>{t('home.unverified')}</p>
          <Link to="/verify-email" className="text-primary underline underline-offset-2">
            {t('home.verify_now')}
          </Link>
        </div>
      )}
      <EmptyState>{t('home.empty')}</EmptyState>
    </div>
  );
}
