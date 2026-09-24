#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-or-later
// API-01：对外可见的路由路径与界面文案中不得出现 subscribe、server、node、traffic。
// 检查范围：各包的语言包（键与值）与路由定义中的 path。
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const webDir = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const banned = /subscri|server|node|traffic/i;

function walk(dir) {
  return readdirSync(dir).flatMap((name) => {
    if (name === 'node_modules' || name === 'dist') return [];
    const p = join(dir, name);
    return statSync(p).isDirectory() ? walk(p) : [p];
  });
}

const problems = [];
for (const pkg of ['portal', 'admin', 'ui']) {
  for (const file of walk(join(webDir, pkg, 'src'))) {
    const rel = relative(webDir, file);
    const text = readFileSync(file, 'utf8');
    if (file.includes(`${join('locales', '')}`) && file.endsWith('.json')) {
      const check = (obj, path) => {
        for (const [k, v] of Object.entries(obj)) {
          const p = `${path}.${k}`;
          if (banned.test(k)) problems.push(`${rel}: 键 ${p}`);
          if (typeof v === 'string' && banned.test(v)) problems.push(`${rel}: ${p} = ${v}`);
          if (typeof v === 'object' && v) check(v, p);
        }
      };
      check(JSON.parse(text), '');
    } else if (/\.tsx?$/.test(file)) {
      for (const m of text.matchAll(/\bpath:\s*['"`]([^'"`]*)['"`]/g)) {
        if (banned.test(m[1])) problems.push(`${rel}: 路由 ${m[1]}`);
      }
    }
  }
}
if (problems.length) {
  console.error(`发现对外禁用词（spec/30 API-01）：\n${problems.join('\n')}`);
  process.exit(1);
}
console.log('check-terms: ok');
