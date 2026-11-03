# 95 - 可观测性

## 日志

- 全量 JSON 到 stdout（`logx.New`），由采集侧负责落盘。
- 固定字段：`time`、`level`、`msg`、`service`、`env`。
- 每请求一次访问日志：`request_id`、`op`、`method`、`path`、`status`、`duration_ms`。
- 业务事件日志：`player_id`、`result_id`、`session_id`、`applied`。
- 5xx 用 `Error` 级别，4xx 用 `Info`（客户端错不是服务端事故）。

## request_id

- 入站：读 `X-Request-Id`，没有就生成 8 字节十六进制。
- 出站：原样回写到响应头；将来跨服务调用时作为 trace 关联键。
- 日志与错误响应共用同一个 id，用户报障时能直接定位。

## 指标

`/metrics` 输出 Prometheus 文本（v0 手写，不引 OpenTelemetry）：

```text
backend_requests_total{service,op,status}    counter
backend_start_time_seconds{service}          gauge
```

业务指标（由 reconciler 每轮发布，spec 0015）：

```text
backend_release_pending_sessions{service}        gauge    待确认回收的会话数（健康时 0）
backend_release_max_attempts{service}            gauge    最严重那场的重试次数
backend_release_oldest_pending_seconds{service}  gauge    最老待回收的等待时长
backend_release_failed_total{service}            counter  累计失败的回收重试
backend_release_confirmed_total{service}         counter  累计成功的回收重试
backend_service_scope_denied_total{service}      counter  强制模式下被 scope 拒绝的调用
backend_service_scope_would_deny_total{service}  counter  观察模式下"本会被拒绝"的调用（应为 0）
```

计数器即使为 0 也会注册：`rate()` 和看板在第一次清扫后就能用，不用等它"先发生过一次"。

新增指标命名规则：`backend_<域>_<单位>`，如 `backend_matchmaking_queue_depth`。
**基数警告**：不要把 `player_id` 放进 label。

## 健康检查的语义

| 端点 | 含义 | 失败时编排器该做什么 |
|---|---|---|
| `/healthz` | 进程活着，能处理 HTTP | 重启 |
| `/readyz` | 依赖就绪（DB/缓存/下游） | 停止导流，不重启 |

- `/healthz` 永远不查下游：数据库慢不该让健康进程被杀。
- `/readyz` 才是查依赖的地方（v0 无依赖，直接 ready）。

## 需要告警的八件事（小团队最小集）

规则文件：`deploy/prometheus/hape.rules.yml`（每 push 由 `ops` job 用 promtool 校验，并有
测试盯着"规则引用的指标确实被注册"）。

| # | 告警 | 表达式要点 |
|---|---|---|
| 1 | `HighServerErrorRatio` | 5xx 比例 > 1%（5 分钟窗口） |
| 2 | `SessionsStuckAllocating` | `allocating` 超过 3 分钟的数量 > 0（分配卡住） |
| 3 | `ZombieSession` | `ready` / `in_progress` 超过 30 分钟的数量 > 0 |
| 4 | `EconomyInternalError` | `economy.*` 出现 500（钱的操作失败一次都值得看） |
| 5 | `DedicatedServerReleaseStuck` | `backend_release_pending_sessions > 0` 持续 15 分钟（厂商按秒计费，这是账单之前唯一的信号） |
| 6 | `UnreleasedAllocation` | `backend_unreleased_allocations > 0` 持续 15 分钟（分配了但没释放，spec 0018 的台账） |
| 7 | `SettlementLatencyDegraded` | 结算 p99 超过阈值（`backend_request_duration_seconds`） |
| 8 | `ServiceScopeDenied` | `backend_service_scope_denied_total` 或观察模式的 `..._would_deny_total` 在 10 分钟内有增长（spec 0016 / 0020） |

**路由**：`deploy/prometheus/alertmanager.yml`（分组 / critical 更快 / receivers 不带 integration，
CI 用 `amtool check-config` 校验）。**还没有的**：抑制与静默策略（需要发布标记）、真实接收人。

## 调试顺序（线上问题）

1. 拿 `request_id` → grep 日志 → 看 `op` 与 `status`。
2. 若是结算问题，按 `result_id` grep，确认扇出三条下游都成功。
3. 若是分配问题，看 `provider` + `allocation_id`，去厂商控制台对账。
4. 任何"钱不对"的怀疑，先看账本 `GET /v1/economy/players/{id}/ledger`，再谈原因。
