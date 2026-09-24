// SPDX-License-Identifier: AGPL-3.0-or-later
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';

export function NotFoundPage() {
  const { t } = useTranslation('common');
  return (
    <main id="main" className="flex min-h-screen flex-col items-center justify-center gap-4 p-6">
      <h1 className="text-xl font-semibold">{t('not_found.title')}</h1>
      <Link to="/" className="text-primary underline underline-offset-2">
        {t('not_found.back_home')}
      </Link>
    </main>
  );
}
