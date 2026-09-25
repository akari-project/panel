// SPDX-License-Identifier: AGPL-3.0-or-later
// 列表页的共用部分：页头、游标分页（CONV-11，“加载更多”）、表格外框与复选框组。
import { useInfiniteQuery, type QueryKey } from '@tanstack/react-query';
import { useId, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, EmptyState, ErrorState, LoadingState } from '@panel/ui';

export function PageHeader({ title, actions, children }: { title: string; actions?: ReactNode; children?: ReactNode }) {
  return (
    <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
      <div className="flex flex-col gap-1">
        <h1 className="text-2xl font-semibold">{title}</h1>
        {children}
      </div>
      {actions && <div className="flex flex-wrap gap-2">{actions}</div>}
    </div>
  );
}

export interface Page<T> {
  items: T[];
  next_cursor?: string | null;
}

/** 游标分页列表：fetchPage 接收游标（首页为 undefined）。 */
export function useCursorList<T>(queryKey: QueryKey, fetchPage: (cursor: string | undefined) => Promise<Page<T>>) {
  const q = useInfiniteQuery({
    queryKey,
    queryFn: ({ pageParam }) => fetchPage(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
  return { ...q, items: q.data?.pages.flatMap((p) => p.items) ?? [] };
}

/** 列表的加载、空、错误三种状态（UI-02）与“加载更多”。 */
export function CursorListView<T>({
  list,
  empty,
  children,
}: {
  list: ReturnType<typeof useCursorList<T>>;
  empty?: ReactNode;
  children: (items: T[]) => ReactNode;
}) {
  const { t } = useTranslation();
  if (list.isPending) return <LoadingState />;
  // 加载后续页失败时保留已加载的条目，只在列表下方提示。
  if (list.isError && !list.isFetchNextPageError) return <ErrorState error={list.error} onRetry={() => void list.refetch()} />;
  if (list.items.length === 0) return <EmptyState>{empty}</EmptyState>;
  return (
    <div className="flex flex-col gap-3">
      {children(list.items)}
      {list.isFetchNextPageError && <ErrorState error={list.error} onRetry={() => void list.fetchNextPage()} />}
      {list.hasNextPage && (
        <div>
          <Button variant="secondary" loading={list.isFetchingNextPage} onClick={() => void list.fetchNextPage()}>
            {t('list.load_more')}
          </Button>
        </div>
      )}
    </div>
  );
}

/** 表格外框：窄屏下表格在框内横向滚动，页面本身不横向滚动。 */
export function TableFrame({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="overflow-x-auto rounded-lg border border-border">
      <table aria-label={label} className="w-full min-w-[40rem] text-left text-sm">
        {children}
      </table>
    </div>
  );
}

export const th = 'border-b border-border bg-surface px-3 py-2 font-medium whitespace-nowrap';
export const td = 'border-b border-border px-3 py-2 align-top';

export interface CheckboxOption {
  value: string;
  label: ReactNode;
  description?: ReactNode;
}

/** 复选框组：fieldset + legend，错误通过 aria-describedby 关联（UI-05）。 */
export function CheckboxGroup({
  legend,
  options,
  value,
  onChange,
  error,
}: {
  legend: string;
  options: CheckboxOption[];
  value: readonly string[];
  onChange: (next: string[]) => void;
  error?: string | undefined;
}) {
  const errorId = useId();
  return (
    <fieldset aria-describedby={error ? errorId : undefined} aria-invalid={error ? true : undefined} className="flex flex-col gap-1">
      <legend className="mb-1 text-sm font-medium">{legend}</legend>
      {options.map((o) => (
        <label key={o.value} className="flex min-h-9 items-start gap-2 py-1 text-sm">
          <input
            type="checkbox"
            className="mt-0.5 size-4 accent-primary"
            checked={value.includes(o.value)}
            onChange={(e) => onChange(e.target.checked ? [...value, o.value] : value.filter((v) => v !== o.value))}
          />
          <span className="flex flex-col">
            <span>{o.label}</span>
            {o.description && <span className="text-muted">{o.description}</span>}
          </span>
        </label>
      ))}
      {error && (
        <p id={errorId} className="text-sm text-danger">
          {error}
        </p>
      )}
    </fieldset>
  );
}
