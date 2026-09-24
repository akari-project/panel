#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-or-later
// 把 panel-spec 指定 tag 的两份 OpenAPI 拷贝到 openapi/，并在 openapi/lock.json 记录 tag、提交与 SHA-256。
// 用法：pnpm sync-openapi [--ref v0.2.0]
// 来源优先级：环境变量 PANEL_SPEC_DIR 指向的本地仓库 → 工作区中的 panel-spec → GitHub。
// 两种来源都先把 ref 解析为完整提交 SHA，再按该提交取文件，因此对同一 ref 得到相同的 lock.json。
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { resolveLocalCommit, resolveRemoteCommit } from './openapi-ref.mjs';

const webDir = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const lockPath = join(webDir, 'openapi', 'lock.json');
const lock = JSON.parse(readFileSync(lockPath, 'utf8'));

const refArg = process.argv.indexOf('--ref');
const ref = refArg > 0 ? process.argv[refArg + 1] : lock.ref;
if (!ref) throw new Error('缺少 --ref');

function findLocalRepo() {
  const candidates = [process.env.PANEL_SPEC_DIR];
  // 从 web/ 向上查找与 panel 同级的 panel-spec（兼容 git worktree 的更深目录）。
  for (let dir = webDir; dir !== dirname(dir); dir = dirname(dir)) {
    candidates.push(join(dir, 'panel-spec'));
  }
  return candidates.find((d) => d && existsSync(join(d, '.git')));
}

async function fetchFile(repo, commit, path) {
  if (repo) {
    return execFileSync('git', ['-C', repo, 'show', `${commit}:${path}`], { encoding: 'utf8' });
  }
  const url = `https://raw.githubusercontent.com/${lock.repository}/${commit}/${path}`;
  const res = await fetch(url);
  if (!res.ok) throw new Error(`${url}: HTTP ${res.status}`);
  return res.text();
}

const repo = findLocalRepo();
const commit = repo ? resolveLocalCommit(repo, ref) : resolveRemoteCommit(lock.repository, ref);

const files = {};
for (const [name, path] of Object.entries(lock.sources)) {
  const text = await fetchFile(repo, commit, path);
  writeFileSync(join(webDir, 'openapi', name), text);
  files[name] = createHash('sha256').update(text).digest('hex');
}

const next = { ...lock, ref, commit, files };
writeFileSync(lockPath, `${JSON.stringify(next, null, 2)}\n`);
console.log(`openapi: ${ref} (${commit.slice(0, 12)}) 来自 ${repo ?? 'GitHub'}`);
