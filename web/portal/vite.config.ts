// SPDX-License-Identifier: AGPL-3.0-or-later
import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';
import { panel } from '../tooling/vite-panel.ts';

export default defineConfig({
  plugins: [react(), tailwindcss(), panel({ app: 'portal', mockTarget: 'http://127.0.0.1:4000', devPort: 5173, siteName: 'Akari portal (dev)' })],
});
