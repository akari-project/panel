<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# 节点协议一致性套件

规格：spec/20（20.6 测试要求）、spec/21 AGT-07、AGT-08、AGT-15、spec/22 ACC-02、ACC-03、ACC-08–10。字段以 `panel-spec` v0.2.0 的 `proto/node/v1` 为准。

## 组成

| 目录 | 内容 |
|---|---|
| `e2e/nodewire` | 字节级实现：握手 MAC、会话密钥、信封加密、快照校验和、DNS 凭据加密、升级签名；可靠会话（seq/ack、窗口、重传队列）；故障注入 `FaultPlan`。`TestVectors` 逐项核对 `panel-spec/testdata/node-v1-vectors.json` |
| `e2e/fakeagent` | 模拟 Agent。`TestHandshakeVectors` 以向量输入握手，Hello、HelloAck、上下行密文都与向量一致 |
| `e2e/testgateway` | 测试用控制面端（内存状态），严格按 spec/20 实现控制面一侧 |
| `e2e/conformance` | 本套件 |

## 运行

```bash
make e2e            # 全部 e2e 测试（-race），CI 中运行
make conformance    # 只运行本套件，-v 输出
```

测试向量从 `go.mod` 依赖的 panel-spec 模块目录读取（`go list -m`）；设置 `PANEL_SPEC_DIR` 可指定其他目录。

## 被测对象

用例分两类：

- `TestAgent_*`：以 Agent 为对象，通过 `Subject` 接口运行，对模拟 Agent 与真实 Agent 相同。故障一律从控制面一侧注入（断开连接、丢帧、重复帧、以新 seq 重发同一信封、暂停读取、注入 `hello_reject`、调整控制面时钟），不依赖被测对象内部的钩子，只通过协议观察结果。
- `TestGateway_*`：网关断言，以原始协议客户端检查控制面一侧（见下节）。
- `TestScale_1000Nodes`、`BenchmarkReconnect1000`：同一进程中的 1,000 个模拟节点。

被测对象由 `CONFORMANCE_AGENT` 选择：

| 取值 | 被测对象 |
|---|---|
| `fake`（默认） | 进程内的模拟 Agent；退避按 2% 缩放、上报周期 200 ms、节点方向窗口 64 条，以缩短用时 |
| `exec` | 外部进程（真实 node-agent，M3） |

可选能力以接口表达，被测对象没有实现时相应用例跳过（输出 `SKIP` 与原因）：

| 接口 | 用途 | 真实 Agent 的实现方式（M3） |
|---|---|---|
| `TrafficSource` | 按凭据产生已知字节数的流量，检查报告不丢不重、`report_seq` 规则、WAL 重传、背压 | 通过真实代理客户端发送已知大小的载荷（AGT-12 的计量口径） |
| `ConnectionSource` | 模拟账号连接，检查租约申请、续租与释放 | 建立与关闭真实代理连接 |
| `StateInspector` | 已应用的凭据集合与版本序列，检查“不重”与旧指令不被应用 | 可选；不实现时这些断言只以 `ReportStatus.config_version` 判断 |

### 对真实 Agent 运行

```bash
make conformance CONFORMANCE_AGENT=exec \
  CONFORMANCE_AGENT_CMD='/path/to/node-agent run --server {server} --enroll-token {token} --state-dir {state_dir}' \
  CONFORMANCE_AGENT_RESTART_CMD='/path/to/node-agent run --state-dir {state_dir}'
```

- 占位符：`{server}` 为测试用控制面端的 `https://` 地址（spec/21 21.5 的 `--server`），`{token}` 为接入令牌（重启与克隆时为空串），`{state_dir}` 为每个实例独立的本地状态目录（AGT-05），`{ca_file}` 为测试证书；证书同时以 `SSL_CERT_FILE` 传给进程。
- 命令以 `sh -c` 执行；停止时向进程组发送 SIGTERM，最多等待 35 秒（spec/21 21.4）。`Restart` 以同一状态目录重新执行 `CONFORMANCE_AGENT_RESTART_CMD`（未设置时用 `CONFORMANCE_AGENT_CMD`）；`Clone` 复制状态目录后启动第二个进程（NODE-21）。
- 时间参数：`CONFORMANCE_BACKOFF_SCALE`（默认 1；Agent 提供测试用的退避缩放时相应设置）、`CONFORMANCE_STATUS_INTERVAL`（默认 30s）、`CONFORMANCE_PATIENCE`（单步等待上限，默认 120s）、`CONFORMANCE_WINDOW`（节点方向窗口，默认 1000）。
- 要让 `TrafficSource` 等可选接口对真实 Agent 生效，在本目录新增一个包装 `ExecSubject` 的实现（例如经本地代理客户端产生流量），并在 `SelectSubject` 中登记。
- 真实 Agent 以默认参数运行时，`hourly` 类用例的探测窗口为 3 秒，断线重连首轮等待不超过 `min(30s, 1s × 2ⁿ)`，全套用时约 10–20 分钟。

## 网关断言与 M2-02

控制面一侧目前没有 `internal/gateway` 实现，由 `e2e/testgateway` 充当 Agent 的对端。它是参照实现，与真实 gateway 的差异：nonce 防重放用内存表代替 Valkey `SET NX EX 180`（NODE-09）；入账只做 `report_seq` 去重与按凭据累加（不做倍率、归属、超额，ACC-03）；租约按固定额度发放；握手超时等参数可以缩短。

以下断言属于控制面行为，M2-02 实现 gateway 后改为对真实 gateway 运行（启动 `panel gateway` 与 PostgreSQL、Valkey，替换 `newGatewayEnv` 与 `newEnv` 中的 `testgateway.Start`），断言本身不变：

| 用例 | 规则 |
|---|---|
| `TestGateway_HandshakeChecks` | NODE-08 时钟偏差、NODE-09 重放、NODE-10 MAC（含能力原始字节与未知字段）、协议版本选择（ENG-03） |
| `TestGateway_HandshakeTimeout` | 20.3 第 5 步 |
| `TestGateway_SeqViolations` | NODE-12 重复、跳号、认证失败即关闭 |
| `TestGateway_SupersedeAndTransition` | NODE-21 4007、NODE-03 过渡期与清空 `psk_prev`、NODE-19 4006 |
| `TestGateway_Enrollment` | NODE-02、NODE-18 |
| `TestGateway_Admission` | NODE-07 准入 503 |
| `TestGateway_ReconnectSyncChoice` | NODE-15 增量与全量的选择、保留期、快照校验和；NODE-25 DNS 凭据按会话 PSK 加密 |
| `TestGateway_IngestDedup` | ACC-03 去重与水位、NODE-13 `idem_key` 去重、`HelloAck.last_report_seq` |
| `TestGateway_LeaseIdempotency` | ACC-08、ACC-10 |
| `TestAgent_WindowOverflowFull` 中的 `Overflows` 断言 | NODE-14 控制面方向溢出转全量 |
| `TestAgent_ReconnectSync` 中的同步方式断言 | NODE-15 |

`TestAgent_*` 中其余断言（Agent 的重试策略、版本规则、重传、报告编号）之后继续以 `testgateway` 或真实 gateway 为对端运行。

## 覆盖（spec/20 20.6）

| 要求 | 用例 |
|---|---|
| 握手：重放、时钟偏差、`hello_reject` 每种原因 | `TestAgent_HelloRejectReasons`、`TestAgent_ClockSkewRetries`、`TestAgent_FreshNonces`、`TestGateway_HandshakeChecks` |
| 密钥：重新接入过渡期、立即吊销 | `TestAgent_ReEnrollTransition`、`TestAgent_KeyRevocation`、`TestGateway_SupersedeAndTransition` |
| 连接：同一节点两条连接 | `TestAgent_TwoAgentsSameNode` |
| 投递：断线 100 次不丢不重；旧版本指令晚于全量到达时不被应用 | `TestAgent_Delivery100Disconnects`、`TestAgent_DeliveryUnderFrameFaults`、`TestAgent_StaleCommandsIgnored`、`TestAgent_VersionGapRequestsFull`、`TestAgent_SnapshotChecksum` |
| 同步：窗口溢出转全量；中间含非凭据变更时发全量 | `TestAgent_WindowOverflowFull`、`TestAgent_ReconnectSync`、`TestGateway_ReconnectSyncChoice` |
| 规模：1,000 个模拟节点同时重连 | `TestScale_1000Nodes`、`BenchmarkReconnect1000` |
| 会话加密测试向量 | `nodewire.TestVectors`、`fakeagent.TestHandshakeVectors` |
| 上报与计量 | `TestAgent_StatusReports`、`TestAgent_TrafficExactlyOnce`、`TestAgent_ReinstallReportSeq`、`TestAgent_ReportBackpressure` |
| 租约 | `TestAgent_LeaseLifecycle`、`TestAgent_NoReleaseWithoutCapability` |
| 凭据与内核 | `TestAgent_InvalidCredentialLength`（AGT-15）、`TestAgent_KernelSwitchAndCapabilities`（AGT-07、AGT-08） |

## 规模测试结果

`TestScale_1000Nodes`：1,000 个模拟 Agent 与测试用控制面端在同一进程中，经本机 TLS WebSocket 连接；模拟 Agent 使用规格默认参数（30 秒上报、NODE-07 退避）。协程数与堆内存包含两端（每个节点：Agent 连接循环、读、写 3 个协程；控制面处理、读、写 3 个协程）。设置 `CONFORMANCE_SCALE_OUT=<文件>` 输出 JSON，`CONFORMANCE_SCALE_NODES` 修改节点数。

测量环境：Intel Core Ultra 7 265K（20 线程）、30 GiB 内存、Linux 6.12、Go 1.27.1，2026-09-24。

| 项目 | `-race` | 不带 `-race` |
|---|---|---|
| 1,000 个节点首次接入并收敛 | 1.49 s | 0.33 s |
| 同时断开后全部重连并收敛 | 1.37 s | 1.00 s |
| 向全部节点各下发一条凭据变更并收敛 | 0.06 s | 0.01 s |
| 协程数（两端合计） | 6,001（6.0 / 节点） | 6,001 |
| 堆内存增量（两端合计） | 83.2 MiB（85 KiB / 节点） | 81.4 MiB（83 KiB / 节点） |
| 进程 Sys | 188 MiB | 159 MiB |
| `BenchmarkReconnect1000`（每次迭代） | 1.40 s | 1.02 s |

重连用时主要是 NODE-07 的退避 `random(0, 1s)`。收敛的判据与 spec/42 42.4 相同：全部节点 `online`，且当前会话上报的已应用版本等于控制面当前值。
