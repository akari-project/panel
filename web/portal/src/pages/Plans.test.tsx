// SPDX-License-Identifier: AGPL-3.0-or-later
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { fakeServer, json, me, renderPortal } from '../test/harness';

const plans = {
  items: [
    {
      id: '0192f0c4-1a00-7000-8000-00000000a001',
      name: '标准版',
      description: '适合日常使用',
      kind: 'recurring',
      tier: 1,
      bytes_per_cycle: 214748364800,
      device_limit: 3,
      speed_limit_mbps: null,
      reset_policy: 'purchase_anchor',
      location_count: 12,
      prices: [
        { id: 'b1', period: 'month', period_days: null, amount_minor: 3000, currency: 'CNY' },
        { id: 'b2', period: 'year', period_days: null, amount_minor: 30000, currency: 'CNY' },
      ],
    },
    {
      id: '0192f0c4-1a00-7000-8000-00000000a002',
      name: '流量包 90 天',
      kind: 'one_time',
      tier: 1,
      bytes_per_cycle: 1099511627776,
      device_limit: 1,
      speed_limit_mbps: 100,
      reset_policy: 'never',
      location_count: 5,
      prices: [{ id: 'b3', period: 'one_time', period_days: 90, amount_minor: 5000, currency: 'CNY' }],
    },
    {
      id: '0192f0c4-1a00-7000-8000-00000000a003',
      name: '免费版',
      kind: 'free',
      tier: 0,
      bytes_per_cycle: 0,
      device_limit: 1,
      speed_limit_mbps: null,
      reset_policy: 'calendar_month',
      location_count: 1,
      prices: [],
    },
  ],
  addon_prices: [],
};

describe('套餐页（spec/32 32.2）', () => {
  it('对比在售套餐，按周期显示价格；流量以 IEC 单位显示；购买按钮暂不可用', async () => {
    renderPortal(fakeServer({ 'GET /v1/me': () => json(200, me()), 'GET /v1/plans': () => json(200, plans) }), '/plans');
    const user = userEvent.setup();
    const standard = await screen.findByRole('article', { name: '标准版' });
    expect(standard).toHaveTextContent('¥30.00/ 月');
    expect(within(standard).getByText('200 GiB')).toBeInTheDocument();
    expect(within(standard).getByText('最多 3 台')).toBeInTheDocument();
    expect(within(standard).getByText('12')).toBeInTheDocument();
    expect(within(standard).getByRole('button', { name: '购买' })).toBeDisabled();

    // 周期只列出有价格的周期；切换后显示对应价格。
    const picker = screen.getByRole('group', { name: '付费周期' });
    expect(within(picker).getAllByRole('radio')).toHaveLength(2);
    await user.click(within(picker).getByLabelText('年付'));
    expect(standard).toHaveTextContent('¥300.00/ 年');

    const oneTime = screen.getByRole('article', { name: '流量包 90 天' });
    expect(oneTime).toHaveTextContent('¥50.00有效 90 天');
    expect(within(oneTime).getByText('1 TiB')).toBeInTheDocument();
    expect(within(oneTime).getByText('100 Mbps')).toBeInTheDocument();

    const free = screen.getByRole('article', { name: '免费版' });
    expect(free).toHaveTextContent('免费');
    expect(within(free).getByText('不限')).toBeInTheDocument();
    expect(within(free).queryByRole('button', { name: '购买' })).not.toBeInTheDocument();
  });

  it('没有在售套餐时显示空状态', async () => {
    renderPortal(
      fakeServer({ 'GET /v1/me': () => json(200, me()), 'GET /v1/plans': () => json(200, { items: [], addon_prices: [] }) }),
      '/plans',
      'en',
    );
    expect(await screen.findByText('No plans are on sale right now.')).toBeInTheDocument();
  });
});
