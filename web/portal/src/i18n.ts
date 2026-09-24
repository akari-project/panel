// SPDX-License-Identifier: AGPL-3.0-or-later
import { createI18n, type Language } from '@panel/ui';
import en from './locales/en/portal.json';
import zh from './locales/zh-CN/portal.json';

export function createPortalI18n(lng?: Language) {
  return createI18n({ resources: { 'zh-CN': { portal: zh }, en: { portal: en } }, defaultNS: 'portal', ...(lng ? { lng } : {}) });
}
