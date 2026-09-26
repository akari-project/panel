// SPDX-License-Identifier: AGPL-3.0-or-later
// 套餐的新建与编辑表单（spec/11 BIL-21、BIL-26）。流量以 GiB 或 TiB 输入，按 1024 进位换算为字节并向下取整（CONV-33）。
// 提交时只把与原值不同的字段交给调用方；服务端的字段错误关联到对应输入框（UI-05）。
import { zodResolver } from '@hookform/resolvers/zod';
import { useState, type ReactNode } from 'react';
import { useForm } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { isProblemError, type Problem } from '@panel/sdk';
import {
  Button,
  ProblemAlert,
  SelectField,
  TextAreaField,
  TextField,
  applyFieldErrors,
  byteInputUnits,
  bytesToInput,
  parseBytesInput,
  type ByteInputUnit,
} from '@panel/ui';
import { planKinds, planStatuses, resetPolicies, type Plan, type PlanKind, type PlanStatus } from '../plans';

const INT32_MAX = 2 ** 31 - 1;

/** 整数输入：可选的负号（仅 allowNegative）+ 数字，范围 [min, INT32_MAX]。 */
function intField(min: number, allowNegative = false) {
  return z
    .string()
    .trim()
    .regex(allowNegative ? /^-?\d+$/ : /^\d+$/, { error: 'plans.validation.integer' })
    .refine((v) => Number(v) >= min && Number(v) <= INT32_MAX, { error: 'field_errors.out_of_range' });
}

const schema = z
  .object({
    name: z.string().trim().min(1, { error: 'validation.required' }).max(100, { error: 'field_errors.too_long' }),
    description: z.string().max(2000, { error: 'field_errors.too_long' }),
    kind: z.enum(planKinds),
    status: z.enum(planStatuses),
    tier: intField(0),
    bytes: z.string(),
    bytes_unit: z.enum(Object.keys(byteInputUnits) as [ByteInputUnit, ...ByteInputUnit[]]),
    device_limit: intField(1),
    speed_limit_mbps: z.union([z.literal(''), intField(1)]),
    reset_policy: z.enum(resetPolicies),
    is_legacy_renew_allowed: z.boolean(),
    sort: intField(-INT32_MAX, true),
  })
  .superRefine((v, ctx) => {
    // kind 与 tier 必须匹配：免费套餐为 0，其他大于 0（BIL-26）。
    if (/^\d+$/.test(v.tier.trim()) && (v.kind === 'free') !== (Number(v.tier) === 0)) {
      ctx.addIssue({ code: 'custom', path: ['tier'], message: 'plans.validation.tier_kind' });
    }
    const b = parseBytesInput(v.bytes, v.bytes_unit);
    if (!b.ok) ctx.addIssue({ code: 'custom', path: ['bytes'], message: b.code === 'out_of_range' ? 'field_errors.out_of_range' : 'plans.validation.decimal' });
  });

export type PlanFormValues = z.infer<typeof schema>;

/** 表单对应的接口字段（PlanCreate / PlanUpdate，不含线路组）。 */
export interface PlanFields {
  name: string;
  description: string | null;
  kind: PlanKind;
  status: PlanStatus;
  tier: number;
  bytes_per_cycle: number;
  device_limit: number;
  speed_limit_mbps: number | null;
  reset_policy: Plan['reset_policy'];
  is_legacy_renew_allowed: boolean;
  sort: number;
}

export function toFields(v: PlanFormValues): PlanFields {
  const b = parseBytesInput(v.bytes, v.bytes_unit);
  return {
    name: v.name.trim(),
    description: v.description.trim() || null,
    kind: v.kind,
    status: v.status,
    tier: Number(v.tier),
    bytes_per_cycle: b.ok ? b.value : 0,
    device_limit: Number(v.device_limit),
    speed_limit_mbps: v.speed_limit_mbps === '' ? null : Number(v.speed_limit_mbps),
    reset_policy: v.reset_policy,
    is_legacy_renew_allowed: v.is_legacy_renew_allowed,
    sort: Number(v.sort),
  };
}

/** 与原套餐不同的字段（PATCH 只提交修改过的字段）。 */
export function changedFields(plan: Plan, next: PlanFields): Partial<PlanFields> {
  const out: Partial<PlanFields> = {};
  for (const k of Object.keys(next) as (keyof PlanFields)[]) {
    if ((plan[k] ?? null) !== next[k]) (out as Record<string, unknown>)[k] = next[k];
  }
  return out;
}

function defaults(plan?: Plan): PlanFormValues {
  const bytes = bytesToInput(plan?.bytes_per_cycle ?? 0);
  return {
    name: plan?.name ?? '',
    description: plan?.description ?? '',
    kind: plan?.kind ?? 'recurring',
    status: plan?.status ?? 'draft',
    tier: String(plan?.tier ?? 1),
    bytes: plan ? bytes.value : '',
    bytes_unit: bytes.unit,
    device_limit: String(plan?.device_limit ?? 3),
    speed_limit_mbps: plan?.speed_limit_mbps ? String(plan.speed_limit_mbps) : '',
    reset_policy: plan?.reset_policy ?? 'purchase_anchor',
    is_legacy_renew_allowed: plan?.is_legacy_renew_allowed ?? true,
    sort: String(plan?.sort ?? 0),
  };
}

// 接口字段名到表单字段名。
const fieldMap = {
  name: 'name',
  description: 'description',
  kind: 'kind',
  status: 'status',
  tier: 'tier',
  bytes_per_cycle: 'bytes',
  device_limit: 'device_limit',
  speed_limit_mbps: 'speed_limit_mbps',
  reset_policy: 'reset_policy',
  sort: 'sort',
} as const;

export interface PlanFormProps {
  plan?: Plan;
  /** 类型已不能修改（已有价格行或权益，BIL-26）。 */
  kindLocked?: boolean;
  submitLabel: string;
  /** 提交；抛出的 ProblemError 关联到字段或显示在表单下方。 */
  onSubmit: (fields: PlanFields) => Promise<void>;
  onCancel?: () => void;
  /** 表单级错误的具体文案。 */
  problemMessage?: (p: Problem) => string | undefined;
  /** 显示在错误提示下方的附加操作（如版本冲突时重新加载）。 */
  problemAction?: (p: Problem) => ReactNode;
}

export function PlanForm({ plan, kindLocked, submitLabel, onSubmit, onCancel, problemMessage, problemAction }: PlanFormProps) {
  const { t } = useTranslation();
  const [problem, setProblem] = useState<Problem | null>(null);
  const { register, handleSubmit, setError, formState } = useForm<PlanFormValues>({
    resolver: zodResolver(schema),
    defaultValues: defaults(plan),
  });
  const err = (k: keyof PlanFormValues) => {
    const m = formState.errors[k]?.message;
    return m ? t(m) : undefined;
  };
  const fieldMessage = (field: string, code: string) =>
    t(`plans.field_errors.${field}.${code}`, { defaultValue: '' }) || undefined;

  const submit = handleSubmit(async (v) => {
    setProblem(null);
    try {
      await onSubmit(toFields(v));
    } catch (e) {
      if (!isProblemError(e)) throw e;
      setProblem(applyFieldErrors(e.problem, fieldMap, setError, t, fieldMessage));
    }
  });

  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <TextField label={t('plans.name')} autoComplete="off" maxLength={100} error={err('name')} {...register('name')} />
      <TextAreaField label={t('plans.description')} error={err('description')} {...register('description')} />
      <div className="grid gap-4 sm:grid-cols-2">
        {kindLocked && plan ? (
          // 类型不可修改时只显示（禁用的输入不会随表单提交，取值仍为原类型）。
          <TextField label={t('plans.kind')} readOnly className="bg-surface text-muted" value={t(`plans.kinds.${plan.kind}`)} hint={t('plans.kind_locked')} />
        ) : (
          <SelectField label={t('plans.kind')} hint={t('plans.kind_hint')} error={err('kind')} {...register('kind')}>
            {planKinds.map((k) => (
              <option key={k} value={k}>
                {t(`plans.kinds.${k}`)}
              </option>
            ))}
          </SelectField>
        )}
        <TextField label={t('plans.tier')} inputMode="numeric" hint={t('plans.tier_hint')} error={err('tier')} {...register('tier')} />
      </div>
      {plan && (
        <SelectField label={t('plans.status')} hint={t('plans.status_hint')} error={err('status')} {...register('status')}>
          {planStatuses.map((s) => (
            <option key={s} value={s}>
              {t(`plans.statuses.${s}`)}
            </option>
          ))}
        </SelectField>
      )}
      <div className="grid grid-cols-[1fr_auto] items-start gap-2">
        <TextField label={t('plans.bytes')} inputMode="decimal" hint={t('plans.bytes_hint')} error={err('bytes')} {...register('bytes')} />
        <SelectField label={t('plans.bytes_unit')} {...register('bytes_unit')}>
          {Object.keys(byteInputUnits).map((u) => (
            <option key={u} value={u}>
              {u}
            </option>
          ))}
        </SelectField>
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <TextField label={t('plans.device_limit')} inputMode="numeric" error={err('device_limit')} {...register('device_limit')} />
        <TextField
          label={t('plans.speed_limit')}
          inputMode="numeric"
          hint={t('plans.speed_limit_hint')}
          error={err('speed_limit_mbps')}
          {...register('speed_limit_mbps')}
        />
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <SelectField label={t('plans.reset_policy')} error={err('reset_policy')} {...register('reset_policy')}>
          {resetPolicies.map((r) => (
            <option key={r} value={r}>
              {t(`plans.reset_policies.${r}`)}
            </option>
          ))}
        </SelectField>
        <TextField label={t('plans.sort')} inputMode="numeric" hint={t('plans.sort_hint')} error={err('sort')} {...register('sort')} />
      </div>
      <label className="flex min-h-9 items-start gap-2 text-sm">
        <input type="checkbox" className="mt-0.5 size-4 accent-primary" {...register('is_legacy_renew_allowed')} />
        <span className="flex flex-col">
          <span>{t('plans.legacy_renew')}</span>
          <span className="text-muted">{t('plans.legacy_renew_hint')}</span>
        </span>
      </label>
      {problem && (
        <div className="flex flex-col gap-2">
          <ProblemAlert problem={problem} message={problemMessage?.(problem)} />
          {problemAction?.(problem)}
        </div>
      )}
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        {onCancel && (
          <Button variant="secondary" onClick={onCancel} disabled={formState.isSubmitting}>
            {t('common:cancel')}
          </Button>
        )}
        <Button type="submit" loading={formState.isSubmitting}>
          {submitLabel}
        </Button>
      </div>
    </form>
  );
}
