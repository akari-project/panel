// SPDX-License-Identifier: AGPL-3.0-or-later
// 运行：pnpm test:root（node --test，不访问网络；git 调用以桩代替）。
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { commitFromLsRemote, resolveLocalCommit, resolveRemoteCommit } from './openapi-ref.mjs';

const TAG_OBJECT = '78da96f7be23946f90493b2a865bb6b9e1c45b0c';
const COMMIT = '7cbca5db73e0295b67845b07102f8db3ec14c380';
const OTHER = '1111111111111111111111111111111111111111';

function stubGit(output) {
  const calls = [];
  const git = (args) => {
    calls.push(args);
    if (output instanceof Error) throw output;
    return output;
  };
  return { git, calls };
}

test('annotated tag 取 ^{} 指向的提交，而不是 tag 对象', () => {
  const out = `${TAG_OBJECT}\trefs/tags/v0.2.1\n${COMMIT}\trefs/tags/v0.2.1^{}\n`;
  assert.equal(commitFromLsRemote(out, 'v0.2.1'), COMMIT);
});

test('lightweight tag 直接取提交', () => {
  assert.equal(commitFromLsRemote(`${COMMIT}\trefs/tags/v0.2.1\n`, 'v0.2.1'), COMMIT);
});

test('分支', () => {
  assert.equal(commitFromLsRemote(`${COMMIT}\trefs/heads/main\n`, 'main'), COMMIT);
});

test('同名 tag 与分支时取 tag，与 git rev-parse 一致', () => {
  const out = `${OTHER}\trefs/heads/v1\n${COMMIT}\trefs/tags/v1\n`;
  assert.equal(commitFromLsRemote(out, 'v1'), COMMIT);
});

test('只按完整名称匹配，不把 v0.2.10 当作 v0.2.1', () => {
  assert.throws(() => commitFromLsRemote(`${COMMIT}\trefs/tags/v0.2.10\n`, 'v0.2.1'), /找不到 ref v0\.2\.1/);
});

test('找不到 ref 时报错', () => {
  assert.throws(() => commitFromLsRemote('', 'v9.9.9'), /找不到 ref v9\.9\.9/);
});

test('resolveRemoteCommit 以 tag、peeled tag、分支三个模式调用 ls-remote', () => {
  const { git, calls } = stubGit(`${TAG_OBJECT}\trefs/tags/v0.2.1\n${COMMIT}\trefs/tags/v0.2.1^{}\n`);
  assert.equal(resolveRemoteCommit('akari-project/panel-spec', 'v0.2.1', git), COMMIT);
  assert.deepEqual(calls, [
    [
      'ls-remote',
      'https://github.com/akari-project/panel-spec.git',
      'refs/tags/v0.2.1',
      'refs/tags/v0.2.1^{}',
      'refs/heads/v0.2.1',
    ],
  ]);
});

test('resolveRemoteCommit 对完整 SHA 不调用 git', () => {
  const { git, calls } = stubGit('');
  assert.equal(resolveRemoteCommit('akari-project/panel-spec', COMMIT, git), COMMIT);
  assert.equal(calls.length, 0);
});

test('resolveRemoteCommit 在 ls-remote 失败时报错', () => {
  const { git } = stubGit(new Error('Could not resolve host'));
  assert.throws(() => resolveRemoteCommit('akari-project/panel-spec', 'v0.2.1', git), /ls-remote.*Could not resolve host/);
});

test('resolveLocalCommit 解到提交', () => {
  const { git, calls } = stubGit(`${COMMIT}\n`);
  assert.equal(resolveLocalCommit('/src/panel-spec', 'v0.2.1', git), COMMIT);
  assert.deepEqual(calls, [['-C', '/src/panel-spec', 'rev-parse', '--verify', '--quiet', 'v0.2.1^{commit}']]);
});

test('resolveLocalCommit 找不到 ref 时报错', () => {
  const { git } = stubGit(new Error('exit 1'));
  assert.throws(() => resolveLocalCommit('/src/panel-spec', 'v9.9.9', git), /找不到 ref v9\.9\.9/);
});
