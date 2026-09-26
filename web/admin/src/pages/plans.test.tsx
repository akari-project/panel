// SPDX-License-Identifier: AGPL-3.0-or-later
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { fakeServer, json, problem, renderAdmin, superadmin } from '../test/harness';

const PLAN_ID = '01927c3e-8a41-7005-9d3e-5f6a7b8c0005';
const G1 = '01927c3e-8a41-7012-9d3e-5f6a7b8c0012';
const G2 = '01927c3e-8a41-7013-9d3e-5f6a7b8c0013';
const G3 = '01927c3e-8a41-7014-9d3e-5f6a7b8c0014';
const PRICE_ID = '01927c3e-8a41-7007-9d3e-5f6a7b8c0007';

const price = {
  id: PRICE_ID,
  plan_id: PLAN_ID,
  period: 'month',
  period_days: null,
  amount_minor: 3000,
  currency: 'CNY',
  is_on_sale: true,
  created_at: '2026-03-01T12:00:00+08:00',
};

const plan = {
  id: PLAN_ID,
  name: '标准版',
  description: '日常使用',
  tier: 2,
  kind: 'recurring',
  status: 'on_sale',
  bytes_per_cycle: 214748364800,
  device_limit: 3,
  speed_limit_mbps: null,
  reset_policy: 'purchase_anchor',
  is_legacy_renew_allowed: true,
  sort: 10,
  location_group_ids: [G1, G2],
  prices: [price],
  active_entitlement_count: 312,
  created_at: '2026-03-01T12:00:00+08:00',
  updated_at: '2026-09-01T09:00:00+08:00',
};

const group = (id: string, name: string, min_tier: number | null = null) => ({
  id,
  name,
  description: null,
  min_tier,
  host_count: 2,
  plan_ids: [PLAN_ID],
  created_at: '2026-03-01T11:00:00+08:00',
  updated_at: '2026-08-15T16:30:00+08:00',
});
const groups = { items: [group(G1, '亚太标准'), group(G2, '欧美', 3), { ...group(G3, '日本专线'), plan_ids: [] }], next_cursor: null };

const impact = (n: number) => ({ affected_account_count: n, affected_host_count: 6, credential_additions: 0, credential_removals: 0, computed_at: '2026-09-23T10:15:00+08:00' });
const assertion = () => json(201, { mfa_assertion: 'v4.public.assert', expires_at: new Date(Date.now() + 300_000).toISOString() });

describe('套餐列表与新建（spec/32 32.3）', () => {
  it('按状态与类型筛选；新建时流量以 TiB 输入按 1024 进位换算（CONV-33），校验等级与类型', async () => {
    const server = fakeServer(superadmin, {
      'GET /v1/plans': () => json(200, { items: [plan], next_cursor: null }),
      'POST /v1/plans': (body) => json(201, { ...plan, ...(body as object), id: PLAN_ID, status: 'draft', prices: [], location_group_ids: [] }, { ETag: '"p1"' }),
      [`GET /v1/plans/${PLAN_ID}`]: () => json(200, { ...plan, status: 'draft', prices: [], location_group_ids: [], active_entitlement_count: 0 }, { ETag: '"p1"' }),
      [`GET /v1/plans/${PLAN_ID}/prices`]: () => json(200, { items: [], next_cursor: null }),
      'GET /v1/location-groups': () => json(200, groups),
    });
    renderAdmin(server, '/plans');
    const user = userEvent.setup();
    const table = await screen.findByRole('table', { name: '套餐与价格' });
    expect(within(table).getByText('200 GiB')).toBeInTheDocument();
    expect(within(table).getByText(/月付\s+¥30\.00/)).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText('状态'), 'hidden');
    await user.selectOptions(screen.getByLabelText('类型'), 'one_time');
    await waitFor(() => expect(server.calls.at(-1)!.search).toBe('?status=hidden&kind=one_time'));

    await user.click(screen.getByRole('button', { name: '新建套餐' }));
    const dialog = await screen.findByRole('dialog', { name: '新建套餐' });
    await user.type(within(dialog).getByLabelText('名称'), '旗舰版');
    await user.selectOptions(within(dialog).getByLabelText('类型'), 'free');
    await user.clear(within(dialog).getByLabelText('等级'));
    await user.type(within(dialog).getByLabelText('等级'), '3');
    await user.type(within(dialog).getByLabelText('每周期流量'), '1.5');
    await user.selectOptions(within(dialog).getByLabelText('单位'), 'TiB');
    await user.click(within(dialog).getByRole('button', { name: '创建' }));
    expect(await within(dialog).findByText('免费套餐的等级必须为 0，其他套餐的等级必须大于 0')).toBeInTheDocument();
    expect(server.called('POST', '/v1/plans')).toHaveLength(0);

    await user.selectOptions(within(dialog).getByLabelText('类型'), 'recurring');
    await user.click(within(dialog).getByRole('button', { name: '创建' }));
    expect(await screen.findByRole('heading', { name: '标准版', level: 1 })).toBeInTheDocument();
    const [post] = server.called('POST', '/v1/plans');
    expect(post!.body).toEqual({
      name: '旗舰版',
      description: null,
      kind: 'recurring',
      tier: 3,
      bytes_per_cycle: 1649267441664,
      device_limit: 3,
      speed_limit_mbps: null,
      reset_policy: 'purchase_anchor',
      is_legacy_renew_allowed: true,
      sort: 0,
    });
    expect(post!.headers.get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/);
  });
});

describe('套餐编辑（BIL-26、UI-03、CONV-28）', () => {
  it('修改等级前显示受影响人数；只提交修改的字段并带 If-Match；版本冲突时提示重新加载', async () => {
    let etag = '"v1"';
    const server = fakeServer(superadmin, {
      [`GET /v1/plans/${PLAN_ID}`]: () => json(200, plan, { ETag: etag }),
      [`GET /v1/plans/${PLAN_ID}/prices`]: () => json(200, { items: [price], next_cursor: null }),
      'GET /v1/location-groups': () => json(200, groups),
      [`POST /v1/plans/${PLAN_ID}/impact`]: () => json(200, impact(312)),
      [`PATCH /v1/plans/${PLAN_ID}`]: () => problem(409, 'conflict'),
    });
    renderAdmin(server, `/plans/${PLAN_ID}`);
    const user = userEvent.setup();
    const basic = await screen.findByRole('region', { name: '基本信息' });
    // 已有价格行，类型只读。
    expect(within(basic).getByLabelText('类型')).toHaveAttribute('readonly');
    expect(within(basic).getByLabelText('每周期流量')).toHaveValue('200');
    await user.clear(within(basic).getByLabelText('等级'));
    await user.type(within(basic).getByLabelText('等级'), '3');
    await user.clear(within(basic).getByLabelText('设备上限'));
    await user.type(within(basic).getByLabelText('设备上限'), '5');
    await user.click(within(basic).getByRole('button', { name: '保存' }));

    const confirm = await screen.findByRole('dialog', { name: '确认修改套餐？' });
    expect(confirm).toHaveTextContent('将影响 312 名用户。');
    expect(server.called('POST', `/v1/plans/${PLAN_ID}/impact`)[0]!.body).toEqual({ tier: 3 });
    await user.click(within(confirm).getByRole('button', { name: '确认修改' }));

    expect(await within(basic).findByText('套餐已被其他管理员修改，请重新加载后再修改。')).toBeInTheDocument();
    const [patch] = server.called('PATCH', `/v1/plans/${PLAN_ID}`);
    expect(patch!.body).toEqual({ tier: 3, device_limit: 5 });
    expect(patch!.headers.get('If-Match')).toBe('"v1"');

    etag = '"v2"';
    await user.click(within(basic).getByRole('button', { name: '重新加载' }));
    await waitFor(() => expect(within(screen.getByRole('region', { name: '基本信息' })).getByLabelText('等级')).toHaveValue('2'));
  });

  it('取消影响确认时不提交', async () => {
    const server = fakeServer(superadmin, {
      [`GET /v1/plans/${PLAN_ID}`]: () => json(200, plan, { ETag: '"v1"' }),
      [`GET /v1/plans/${PLAN_ID}/prices`]: () => json(200, { items: [price], next_cursor: null }),
      'GET /v1/location-groups': () => json(200, groups),
      [`POST /v1/plans/${PLAN_ID}/impact`]: () => json(200, impact(312)),
    });
    renderAdmin(server, `/plans/${PLAN_ID}`);
    const user = userEvent.setup();
    const basic = await screen.findByRole('region', { name: '基本信息' });
    await user.selectOptions(within(basic).getByLabelText('状态'), 'archived');
    await user.click(within(basic).getByRole('button', { name: '保存' }));
    const confirm = await screen.findByRole('dialog', { name: '确认修改套餐？' });
    expect(confirm).toHaveTextContent('下架后不能新购');
    await user.click(within(confirm).getByRole('button', { name: '取消' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(server.called('PATCH', `/v1/plans/${PLAN_ID}`)).toHaveLength(0);
  });
});

describe('套餐的线路组（BIL-04、AUTH-19）', () => {
  it('添加不需确认；移除前预览影响，经重新验证后带原因、Mfa-Assertion 与 If-Match', async () => {
    const server = fakeServer(superadmin, {
      [`GET /v1/plans/${PLAN_ID}`]: () => json(200, plan, { ETag: '"v1"' }),
      [`GET /v1/plans/${PLAN_ID}/prices`]: () => json(200, { items: [price], next_cursor: null }),
      'GET /v1/location-groups': () => json(200, groups),
      [`PUT /v1/plans/${PLAN_ID}/location-groups/${G3}`]: () => json(200, { ...plan, location_group_ids: [G1, G2, G3] }, { ETag: '"v2"' }),
      [`POST /v1/plans/${PLAN_ID}/impact`]: () => json(200, impact(120)),
      'POST /v1/staff/me/step-up': () => assertion(),
      [`DELETE /v1/plans/${PLAN_ID}/location-groups/${G1}`]: () => json(200, { ...plan, location_group_ids: [G2, G3] }, { ETag: '"v3"' }),
    });
    renderAdmin(server, `/plans/${PLAN_ID}`);
    const user = userEvent.setup();
    const section = await screen.findByRole('region', { name: '线路组' });
    expect(await within(section).findByText('亚太标准')).toBeInTheDocument();
    // 欧美的最低等级 3 高于套餐等级 2。
    expect(within(section).getByText('套餐等级 2 低于该组的最低等级 3，用户不会获得该组的节点。')).toBeInTheDocument();

    await user.selectOptions(within(section).getByLabelText('添加线路组'), G3);
    await user.click(within(section).getByRole('button', { name: '添加' }));
    expect(await within(section).findByText('日本专线')).toBeInTheDocument();
    expect(server.called('PUT', `/v1/plans/${PLAN_ID}/location-groups/${G3}`)[0]!.headers.get('If-Match')).toBe('"v1"');

    await user.click(within(section).getByRole('button', { name: '从套餐移除线路组 亚太标准' }));
    const confirm = await screen.findByRole('dialog', { name: '移除线路组 亚太标准？' });
    expect(confirm).toHaveTextContent('将影响 120 名用户。');
    expect(server.called('POST', `/v1/plans/${PLAN_ID}/impact`)[0]!.body).toEqual({ location_group_ids: [G2, G3] });
    await user.type(within(confirm).getByLabelText('操作原因'), '线路下线 50%');
    await user.click(within(confirm).getByRole('button', { name: '移除' }));
    const stepUp = await screen.findByRole('dialog', { name: '验证身份' });
    await user.type(within(stepUp).getByLabelText('验证码'), '492871');
    await user.click(within(stepUp).getByRole('button', { name: '验证' }));
    await waitFor(() => expect(within(section).queryByRole('button', { name: '从套餐移除线路组 亚太标准' })).not.toBeInTheDocument());
    const [del] = server.called('DELETE', `/v1/plans/${PLAN_ID}/location-groups/${G1}`);
    expect(del!.headers.get('Audit-Reason')).toBe(encodeURIComponent('线路下线 50%'));
    expect(del!.headers.get('Mfa-Assertion')).toBe('v4.public.assert');
    expect(del!.headers.get('If-Match')).toBe('"v2"');
  });
});

describe('价格行（BIL-01、CONV-08）', () => {
  it('周期只列出与类型匹配的选项；金额换算为最小单位，币种取站点结算货币；停售带 If-Match，最后一行在售价格给出专门提示', async () => {
    const server = fakeServer(superadmin, {
      [`GET /v1/plans/${PLAN_ID}`]: () => json(200, plan, { ETag: '"v1"' }),
      [`GET /v1/plans/${PLAN_ID}/prices`]: () => json(200, { items: [price], next_cursor: null }),
      'GET /v1/location-groups': () => json(200, groups),
      'GET /v1/settings': () => json(200, { currency: 'CNY' }),
      [`POST /v1/plans/${PLAN_ID}/prices`]: () => json(201, { ...price, id: 'p2', period: 'year', amount_minor: 29990 }),
      [`GET /v1/plans/${PLAN_ID}/prices/${PRICE_ID}`]: () => json(200, price, { ETag: '"pr1"' }),
      [`PATCH /v1/plans/${PLAN_ID}/prices/${PRICE_ID}`]: () => problem(409, 'invalid_state'),
    });
    renderAdmin(server, `/plans/${PLAN_ID}`);
    const user = userEvent.setup();
    const section = await screen.findByRole('region', { name: '价格' });
    expect(await within(section).findByText('¥30.00')).toBeInTheDocument();
    // 价格行没有编辑入口。
    expect(within(section).queryByRole('button', { name: /编辑/ })).not.toBeInTheDocument();

    await user.click(within(section).getByRole('button', { name: '新增价格' }));
    const dialog = await screen.findByRole('dialog', { name: '新增价格' });
    const options = within(within(dialog).getByLabelText('周期')).getAllByRole('option').map((o) => o.textContent);
    expect(options).toEqual(['月付', '季付', '半年付', '年付']);
    expect(within(dialog).queryByLabelText('有效天数')).not.toBeInTheDocument();
    await user.selectOptions(within(dialog).getByLabelText('周期'), 'year');
    await user.type(await within(dialog).findByLabelText('金额（CNY）'), '299.999');
    await user.click(within(dialog).getByRole('button', { name: '创建' }));
    expect(await within(dialog).findByText('金额格式不正确或小数位过多')).toBeInTheDocument();
    await user.clear(within(dialog).getByLabelText('金额（CNY）'));
    await user.type(within(dialog).getByLabelText('金额（CNY）'), '299.9');
    await user.click(within(dialog).getByRole('button', { name: '创建' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    const [post] = server.called('POST', `/v1/plans/${PLAN_ID}/prices`);
    expect(post!.body).toEqual({ period: 'year', amount_minor: 29990, currency: 'CNY' });

    await user.click(within(section).getByRole('button', { name: '停售 月付 ¥30.00' }));
    const confirm = await screen.findByRole('dialog', { name: '停售价格？' });
    await user.click(within(confirm).getByRole('button', { name: '停售' }));
    expect(await within(confirm).findByText('这是在售套餐的最后一个在售价格，请先把套餐改为“隐藏”或“已下架”。')).toBeInTheDocument();
    const [patch] = server.called('PATCH', `/v1/plans/${PLAN_ID}/prices/${PRICE_ID}`);
    expect(patch!.body).toEqual({ is_on_sale: false });
    expect(patch!.headers.get('If-Match')).toBe('"pr1"');
  });

  it('一次性套餐只有一次性周期，可填有效天数；服务端字段错误关联到输入框', async () => {
    const oneTime = { ...plan, kind: 'one_time', prices: [], active_entitlement_count: 0 };
    const server = fakeServer(superadmin, {
      [`GET /v1/plans/${PLAN_ID}`]: () => json(200, oneTime, { ETag: '"v1"' }),
      [`GET /v1/plans/${PLAN_ID}/prices`]: () => json(200, { items: [], next_cursor: null }),
      'GET /v1/location-groups': () => json(200, groups),
      'GET /v1/settings': () => json(200, { currency: 'CNY' }),
      [`POST /v1/plans/${PLAN_ID}/prices`]: () => problem(400, 'invalid_request', { errors: [{ field: 'period_days', code: 'not_allowed' }] }),
    });
    renderAdmin(server, `/plans/${PLAN_ID}`);
    const user = userEvent.setup();
    const section = await screen.findByRole('region', { name: '价格' });
    await user.click(within(section).getByRole('button', { name: '新增价格' }));
    const dialog = await screen.findByRole('dialog', { name: '新增价格' });
    expect(within(within(dialog).getByLabelText('周期')).getAllByRole('option').map((o) => o.textContent)).toEqual(['一次性']);
    await user.type(within(dialog).getByLabelText('有效天数'), '90');
    await user.type(await within(dialog).findByLabelText('金额（CNY）'), '50');
    await user.click(within(dialog).getByRole('button', { name: '创建' }));
    expect(await within(dialog).findByText('只有一次性套餐可以设置有效天数')).toBeInTheDocument();
    expect(server.called('POST', `/v1/plans/${PLAN_ID}/prices`)[0]!.body).toEqual({ period: 'one_time', amount_minor: 5000, currency: 'CNY', period_days: 90 });
  });

  it('免费套餐不显示价格操作', async () => {
    const free = { ...plan, kind: 'free', tier: 0, prices: [] };
    renderAdmin(
      fakeServer(superadmin, {
        [`GET /v1/plans/${PLAN_ID}`]: () => json(200, free, { ETag: '"v1"' }),
        'GET /v1/location-groups': () => json(200, groups),
      }),
      `/plans/${PLAN_ID}`,
    );
    const section = await screen.findByRole('region', { name: '价格' });
    expect(within(section).getByText('免费套餐不设价格。')).toBeInTheDocument();
    expect(within(section).queryByRole('button', { name: '新增价格' })).not.toBeInTheDocument();
  });
});

describe('线路组（ACS-05、ACS-06）', () => {
  it('修改最低等级前显示受影响人数，带 If-Match 提交；被引用时删除给出专门提示', async () => {
    const server = fakeServer(superadmin, {
      'GET /v1/location-groups': () => json(200, groups),
      [`GET /v1/location-groups/${G1}`]: () => json(200, groups.items[0], { ETag: '"g1"' }),
      [`POST /v1/location-groups/${G1}/impact`]: () => json(200, impact(42)),
      [`PATCH /v1/location-groups/${G1}`]: () => json(200, { ...groups.items[0], min_tier: 2 }, { ETag: '"g2"' }),
      [`DELETE /v1/location-groups/${G1}`]: () => problem(409, 'invalid_state'),
      'POST /v1/location-groups': () => json(201, group(G3, '新组')),
    });
    renderAdmin(server, '/location-groups');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '编辑线路组 亚太标准' }));
    const dialog = await screen.findByRole('dialog', { name: '编辑线路组 亚太标准' });
    await user.type(await within(dialog).findByLabelText('最低等级'), '2');
    await user.click(within(dialog).getByRole('button', { name: '保存' }));
    const confirm = await screen.findByRole('dialog', { name: '确认修改最低等级？' });
    expect(confirm).toHaveTextContent('将影响 42 名用户。');
    expect(server.called('POST', `/v1/location-groups/${G1}/impact`)[0]!.body).toEqual({ min_tier: 2 });
    await user.click(within(confirm).getByRole('button', { name: '确认修改' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    const [patch] = server.called('PATCH', `/v1/location-groups/${G1}`);
    expect(patch!.body).toEqual({ min_tier: 2 });
    expect(patch!.headers.get('If-Match')).toBe('"g1"');

    await user.click(screen.getByRole('button', { name: '删除线路组 亚太标准' }));
    const del = await screen.findByRole('dialog', { name: '删除线路组 亚太标准？' });
    expect(del).toHaveTextContent('仍被 1 个套餐使用');
    await user.click(within(del).getByRole('button', { name: '删除' }));
    expect(await within(del).findByText('线路组仍被套餐使用，请先从相关套餐中移除。')).toBeInTheDocument();
    expect(server.called('DELETE', `/v1/location-groups/${G1}`)[0]!.headers.get('If-Match')).toBe('"g1"');

    await user.click(within(del).getByRole('button', { name: '取消' }));
    await user.click(screen.getByRole('button', { name: '新建线路组' }));
    const create = await screen.findByRole('dialog', { name: '新建线路组' });
    await user.type(within(create).getByLabelText('名称'), '新组');
    await user.click(within(create).getByRole('button', { name: '创建' }));
    await waitFor(() => expect(server.called('POST', '/v1/location-groups')).toHaveLength(1));
    expect(server.called('POST', '/v1/location-groups')[0]!.body).toEqual({ name: '新组', description: null, min_tier: null });
  });
});
