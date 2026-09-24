// SPDX-License-Identifier: AGPL-3.0-or-later
import { createI18n, type Language } from '@panel/ui';
import en from './locales/en/admin.json';
import zh from './locales/zh-CN/admin.json';

export function createAdminI18n(lng?: Language) {
  return createI18n({ resources: { 'zh-CN': { admin: zh }, en: { admin: en } }, defaultNS: 'admin', ...(lng ? { lng } : {}) });
}
