// SPDX-License-Identifier: AGPL-3.0-or-later
// 把 panel-spec 的 ref（tag、分支或完整提交 SHA）解析为完整提交 SHA，供 sync-openapi 写入 lock.json。
// 本地仓库用 git rev-parse；GitHub 用 git ls-remote（无需凭据），annotated tag 取 ^{} 指向的提交。
// 两种来源对同一 ref 得到同一提交；解析失败时抛错，不返回 null。
import { execFileSync } from 'node:child_process';

const SHA = /^[0-9a-f]{40}$/;

function defaultGit(args) {
  return execFileSync('git', args, {
    encoding: 'utf8',
    env: { ...process.env, GIT_TERMINAL_PROMPT: '0' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
}

// 从 git ls-remote 的输出中选出 ref 对应的提交。优先级与 git rev-parse 一致：tag 先于分支；
// annotated tag 的对象不是提交，取其 ^{} 行。
export function commitFromLsRemote(output, ref) {
  const refs = new Map();
  for (const line of output.split('\n')) {
    const [sha, name] = line.trim().split(/\s+/);
    if (sha && name) refs.set(name, sha);
  }
  const sha =
    refs.get(`refs/tags/${ref}^{}`) ?? refs.get(`refs/tags/${ref}`) ?? refs.get(`refs/heads/${ref}`);
  if (!sha || !SHA.test(sha)) throw new Error(`GitHub 上找不到 ref ${ref}`);
  return sha;
}

export function resolveRemoteCommit(repository, ref, git = defaultGit) {
  // 完整 SHA 无法用 ls-remote 查询，原样使用；按提交取文件时不存在的提交会返回 404。
  if (SHA.test(ref)) return ref;
  const url = `https://github.com/${repository}.git`;
  let output;
  try {
    output = git(['ls-remote', url, `refs/tags/${ref}`, `refs/tags/${ref}^{}`, `refs/heads/${ref}`]);
  } catch (err) {
    throw new Error(`git ls-remote ${url} 失败：${err.message}`, { cause: err });
  }
  return commitFromLsRemote(output, ref);
}

export function resolveLocalCommit(repo, ref, git = defaultGit) {
  let sha;
  try {
    sha = git(['-C', repo, 'rev-parse', '--verify', '--quiet', `${ref}^{commit}`]).trim();
  } catch (err) {
    throw new Error(`${repo} 中找不到 ref ${ref}`, { cause: err });
  }
  if (!SHA.test(sha)) throw new Error(`${repo} 中找不到 ref ${ref}`);
  return sha;
}
