// SPDX-License-Identifier: AGPL-3.0-or-later
// 从 Mailpit 取邮件（GET /api/v1/search、GET /api/v1/message/{ID}）。
import { expect } from '@playwright/test';

const MAILPIT = (process.env.MAILPIT_URL ?? '').replace(/\/+$/, '');

interface Summary {
  ID: string;
}

async function getJson<T>(path: string): Promise<T> {
  const res = await fetch(`${MAILPIT}${path}`);
  if (!res.ok) throw new Error(`Mailpit ${path}: ${res.status}`);
  return (await res.json()) as T;
}

async function messageIds(to: string): Promise<string[]> {
  const q = encodeURIComponent(`to:"${to}"`);
  const r = await getJson<{ messages: Summary[] | null }>(`/api/v1/search?query=${q}`);
  return (r.messages ?? []).map((m) => m.ID);
}

/** 当前已收到的邮件 ID，用于之后只等待新邮件。 */
export async function seenMessages(to: string): Promise<Set<string>> {
  return new Set(await messageIds(to));
}

/** 等待发往 to 的一封新邮件（不在 seen 中，最新的一封），返回纯文本正文。 */
export async function nextMessageText(to: string, seen: Set<string>): Promise<string> {
  let id: string | undefined;
  await expect
    .poll(
      async () => {
        id = (await messageIds(to)).find((x) => !seen.has(x));
        return id;
      },
      { message: `等待发往 ${to} 的邮件`, timeout: 30_000, intervals: [250, 500, 1000] },
    )
    .toBeTruthy();
  seen.add(id!);
  const m = await getJson<{ Text: string }>(`/api/v1/message/${id}`);
  return m.Text;
}

export function verificationCode(text: string): string {
  const m = /\b(\d{6})\b/.exec(text);
  if (!m) throw new Error(`邮件中没有 6 位验证码：\n${text}`);
  return m[1]!;
}

export function resetLink(text: string): URL {
  const m = /(https?:\/\/\S+?#token=[A-Za-z0-9_-]{43})/.exec(text);
  if (!m) throw new Error(`邮件中没有重置链接：\n${text}`);
  return new URL(m[1]!);
}
