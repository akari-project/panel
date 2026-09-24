// SPDX-License-Identifier: AGPL-3.0-or-later
import js from '@eslint/js';
import jsxA11y from 'eslint-plugin-jsx-a11y';
import reactHooks from 'eslint-plugin-react-hooks';
import globals from 'globals';
import tseslint from 'typescript-eslint';

export default tseslint.config(
  { ignores: ['**/dist/**', '**/node_modules/**', '**/*.gen.ts', 'test-results/**', 'playwright-report/**'] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ['**/*.{ts,tsx}'],
    languageOptions: { globals: globals.browser },
    plugins: { 'react-hooks': reactHooks, 'jsx-a11y': jsxA11y },
    rules: {
      ...reactHooks.configs.recommended.rules,
      ...jsxA11y.flatConfigs.recommended.rules,
      '@typescript-eslint/consistent-type-imports': 'error',
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_' }],
      // UI-01：只通过 @panel/sdk 调用接口，不手写请求。
      'no-restricted-globals': ['error', { name: 'fetch', message: '通过 @panel/sdk 调用接口（UI-01）' }],
      'no-restricted-properties': [
        'error',
        { object: 'window', property: 'fetch', message: '通过 @panel/sdk 调用接口（UI-01）' },
        { object: 'globalThis', property: 'fetch', message: '通过 @panel/sdk 调用接口（UI-01）' },
      ],
    },
  },
  {
    files: ['**/*.test.{ts,tsx}', 'e2e/**', 'sdk/src/**'],
    rules: { 'no-restricted-globals': 'off', 'no-restricted-properties': 'off' },
  },
  {
    files: ['**/*.{js,mjs}', '*.config.ts', '**/vite.config.ts', '**/vitest.config.ts', 'tooling/**'],
    languageOptions: { globals: globals.node },
  },
  {
    files: ['ui/src/theme-init.js'],
    languageOptions: { globals: globals.browser, sourceType: 'script' },
  },
);
