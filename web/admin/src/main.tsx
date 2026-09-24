// SPDX-License-Identifier: AGPL-3.0-or-later
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { createConsoleApi } from '@panel/sdk';
import { applyCspNonce, basePathFromModule, installChunkReload, readPanelConfig } from '@panel/ui';
import { App } from './app';
import { createAdminI18n } from './i18n';
import './styles.css';

const config = readPanelConfig(window.__PANEL_CONFIG__, basePathFromModule(import.meta.url));
applyCspNonce(config.csp_nonce);
installChunkReload();
document.title = config.site_name;

const root = document.getElementById('root');
if (!root) throw new Error('#root 不存在');
createRoot(root).render(
  <StrictMode>
    <App api={createConsoleApi({ baseUrl: config.api_base_url })} config={config} i18n={createAdminI18n()} />
  </StrictMode>,
);
