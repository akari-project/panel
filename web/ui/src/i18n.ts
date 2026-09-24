// SPDX-License-Identifier: AGPL-3.0-or-later
// i18next 初始化（UI-04）。语言包按命名空间拆分：common 由本包提供，各应用提供自己的命名空间。
import i18next, { type i18n, type ResourceLanguage } from 'i18next';
import LanguageDetector from 'i18next-browser-languagedetector';
import { initReactI18next } from 'react-i18next';
import commonEn from './locales/en/common.json';
import commonZh from './locales/zh-CN/common.json';

export const supportedLanguages = ['zh-CN', 'en'] as const;
export type Language = (typeof supportedLanguages)[number];

export const LANGUAGE_STORAGE_KEY = 'panel.lang';

export interface CreateI18nOptions {
  /** 应用命名空间的语言包：{ 'zh-CN': { portal: {...} }, en: { portal: {...} } } */
  resources: Record<Language, ResourceLanguage>;
  defaultNS: string;
  /** 测试中固定语言，跳过检测 */
  lng?: Language;
}

export function createI18n({ resources, defaultNS, lng }: CreateI18nOptions): i18n {
  const instance = i18next.createInstance();
  if (!lng) instance.use(LanguageDetector);
  instance.use(initReactI18next);
  void instance.init({
    resources: {
      'zh-CN': { common: commonZh, ...resources['zh-CN'] },
      en: { common: commonEn, ...resources.en },
    },
    ...(lng ? { lng } : {}),
    supportedLngs: [...supportedLanguages],
    fallbackLng: 'en',
    ns: ['common', defaultNS],
    defaultNS,
    fallbackNS: 'common',
    interpolation: { escapeValue: false },
    detection: {
      order: ['localStorage', 'navigator'],
      lookupLocalStorage: LANGUAGE_STORAGE_KEY,
      caches: ['localStorage'],
    },
    initAsync: false,
  });
  const syncLang = (l: string) => {
    if (typeof document !== 'undefined') document.documentElement.lang = l;
  };
  syncLang(instance.resolvedLanguage ?? instance.language);
  instance.on('languageChanged', syncLang);
  return instance;
}
