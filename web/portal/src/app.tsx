// SPDX-License-Identifier: AGPL-3.0-or-later
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, type RouterHistory } from '@tanstack/react-router';
import type { i18n } from 'i18next';
import { useEffect, useState } from 'react';
import { I18nextProvider } from 'react-i18next';
import { isProblemError, type ClientApi } from '@panel/sdk';
import { ThemeProvider, type PanelConfig } from '@panel/ui';
import { ReauthDialog, type ReauthRequest } from './components/ReauthDialog';
import { createAppRouter, handleSessionExpired } from './router';

export function createQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: {
        // 4xx 不重试；其他错误最多重试 2 次。
        retry: (count, e) => !(isProblemError(e) && e.problem.status >= 400 && e.problem.status < 500) && count < 2,
        refetchOnWindowFocus: false,
      },
    },
  });
}

export interface AppProps {
  api: ClientApi;
  config: PanelConfig;
  i18n: i18n;
  history?: RouterHistory;
}

export function App({ api, config, i18n, history }: AppProps) {
  const [queryClient] = useState(createQueryClient);
  const [router] = useState(() => createAppRouter({ api, config, queryClient }, history));
  const [reauth, setReauth] = useState<ReauthRequest | null>(null);

  // 请求层的会话恢复钩子（UI-09、AUTH-07）。
  useEffect(
    () =>
      api.setSessionHooks({
        reauthenticate: (problem) => new Promise<boolean>((resolve) => setReauth({ problem, resolve })),
        sessionExpired: () => handleSessionExpired(router, queryClient),
      }),
    [api, router, queryClient],
  );

  return (
    <I18nextProvider i18n={i18n}>
      <ThemeProvider>
        <QueryClientProvider client={queryClient}>
          <RouterProvider router={router} />
          <ReauthDialog api={api} request={reauth} onClose={() => setReauth(null)} />
        </QueryClientProvider>
      </ThemeProvider>
    </I18nextProvider>
  );
}
