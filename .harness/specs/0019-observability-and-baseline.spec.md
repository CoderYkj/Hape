# 0019 - M5 前置：观测接线、告警补齐与容量基线

- 状态：implemented
- 日期：2026-11-03
- 关联：spec 0015（回收指标）、0018（分配台账）、rules/95（可观测性）
- 涉及：`services/session`（状态普查）、`platform`（gauge 发布）、`deploy/prometheus/*`、`deploy/docker-compose.prod.yml`、`.github/workflows/ci.yml`、`tools/loadtest`、`pkg/idgen`

## 1. 背景

M5 的目标是"指标落地、压测、SLO"。做完 0015/0018 之后，指标**已经有两个族**，但三件事没做：

1. **没有采集方**：`/metrics` 只有 `curl` 会看；告警规则只有 1 条。
2. **规则里缺两类输入**：`allocating 卡住` 与 `僵尸对局` 需要"最老多久"这个数字，而当时只有状态，没有年龄。
3. **没有容量数字**：任何"能扛多少"的讨论都是猜。

## 2. 目标 / 非目标

目标：

1. 会话状态普查（`allocating/ready/in_progress` 的数量与最老年龄）→ 两个告警的输入。
2. Prometheus 真正接线：抓取配置 + 6 条告警规则 + **CI 用 promtool 校验**。
3. 压测工具 + 一台机器上的基线数字（内存 vs Postgres）。

非目标：

- 不做 SLO 定义与承诺（需要业务给目标，属下一步）。
- 不做多机/真实网络压测（本机 loopback，数字只当**本机基线**）。
- 不引入 tracing / 日志聚合 / 告警路由（Alertmanager 属部署选择）。

## 3. 设计

### 3.1 状态普查

`CensusProvider`（可选 store 能力，与 `Expirer`/`ReleaseTracker` 同构）：

```text
Counts:  allocating / ready / in_progress
Oldest:  各状态里最老会话的创建时长（秒）；空状态为 0
```

PG 用一条 `GROUP BY` 查询；内存实现遍历。`created_at` 被当作"卡住时长"的**上界**——方向对告警有利。
reconciler 每轮发布 6 个 gauge：

```text
backend_sessions_allocating / _ready / _in_progress
backend_oldest_allocating_seconds / _ready_ / _in_progress_
```

### 3.2 告警规则（6 条，全部有指标支撑）

| 告警 | 表达式 | for | 严重级 |
|---|---|---|---|
| `DedicatedServerReleaseStuck` | `backend_release_pending_sessions > 0` | 15m | warning |
| `UnreleasedAllocation` | `backend_unreleased_allocations > 0` | 15m | warning |
| `SessionsStuckAllocating` | `backend_oldest_allocating_seconds > 180` | 5m | warning |
| `ZombieSession` | `backend_oldest_ready_seconds > 1800 or backend_oldest_in_progress_seconds > 1800` | 10m | warning |
| `HighServerErrorRatio` | `sum(rate(requests{5xx}))/max(sum(rate(requests)),0.001) > 0.01` | 5m | critical |
| `EconomyInternalError` | `increase(backend_requests_total{op=~"economy\..*",status="500"}[5m]) > 0` | 1m | critical |

三层防漂移：

1. `promtool check rules/config` 在 CI 里跑（语法 + PromQL 合法性）；
2. `platform` 的漂移测试：**规则里引用的每个指标都必须真的出现在 `/metrics`**（否则告警永远不会响）；
3. 规则文件与指标同仓，改动一起评审。

### 3.3 压测工具与基线

`tools/loadtest` 驱动 N 场完整对局（登录 → 入队 → pop → create → ready → 战报），每场独立队列避免互相配对，输出各阶段 p50/p95/p99 与吞吐。CLI 与 JSON 两种输出。

**基线（本机 Windows，单进程，dev auth，本地 allocator，loopback，无 TLS）**：

| 场景 | 并发 | 对局数 | 成功率 | 吞吐 | total p50 | total p99 |
|---|---|---|---|---|---|---|
| 内存 store | 25 | 400 | 400/400 | **307 场/秒**（18.4k/分） | 80ms | 95ms |
| Postgres store | 10 | 200 | 200/200 | **121 场/秒**（7.3k/分） | 59ms | 537ms |

怎么读：

- 两个场景**并发不同**（25 vs 10），所以 p50 不能横比；同并发下 PG 会更慢。
- PG 的尾巴（p99 537ms，`login` p99 437ms）是连接池与行锁竞争，不是某条慢查询。
- 这些数字的含义是"这台机器、这个形态下的天花板量级"，不是生产容量承诺。

## 4. 压测发现的缺陷（这次前置的真正收获）

首轮压测 **44/300 失败**，全部是 409：`session already exists`（同一个 id）、`player already has a waiting ticket`。
根因：会话/对局/票据/队伍的 ID 是 `UnixNano`（纳秒时间戳），而 Windows 的时钟粒度是毫秒级——
**同一 tick 的并发请求拿到同一个 ID**。内存实现用进程内计数器，所以只有 SQL 路径和真实并发才暴露。

修复：`pkg/idgen` 统一生成 `<prefix><base36 纳秒>-<base36 计数器>-<6 位随机>`：

- 计数器保证单进程内即使时钟停走也不重复；
- 随机后缀覆盖"同一 tick 内重启"与"两台机器时钟一致"；
- 有并发唯一性测试（16 goroutine × 5000 个 ID，零重复）。

替换点：`session.create`、`matchmaking.pop` 的 match id、票据 id、队伍 id。

## 5. 失败模式

| 场景 | 行为 |
|---|---|
| 未开启 `BACKEND_RECONCILE_INTERVAL` | 业务 gauge 不发布；告警不误报但也永不响——部署检查单里必须确认开启 |
| 规则引用不存在的指标 | CI 的漂移测试失败 |
| 规则语法/PromQL 错误 | CI 的 promtool 步骤失败 |
| 时钟停走 / 高并发 | 不会产生重复 ID（计数器兜底） |
| 压测失败率非 0 | 工具按场次报错并计入 `failed`，退出码仍为 0（压测是测量，不是断言） |

## 6. 验证

| 层 | 用例 |
|---|---|
| 状态普查 | `platform` 测试断言 6 个 gauge 都在 `/metrics` 上 |
| 告警漂移 | `TestAlertRulesWatchMetricsWePublish`：解析规则文件，7 个指标全部必须被导出 |
| 规则语法 | CI `ops` job：`promtool check rules` + `promtool check config` |
| ID 唯一性 | `pkg/idgen`：16×5000 并发零重复；前缀与三段式结构 |
| 压测 | 内存 400 场 0 失败、PG 200 场 0 失败；输出各阶段分位数 |
| 回归 | 全量测试 + 契约漂移 + 48 条断言 |

## 7. 回滚

删掉 `pkg/idgen` 的调用点即回到时间戳 ID（会重新引入碰撞，仅用于排查）；Prometheus 是可选 profile，不启动不影响后端。

## 8. 未决

- [ ] **SLO 定义**：需要业务给目标（延迟/可用性/结算时效），本 spec 只提供测量。
- [ ] 持续压测（soak，≥1 小时）与内存/连接泄漏观察。
- [ ] 多机 + 真实网络（RTT、丢包）下的分配与结算延迟。
- [ ] Alertmanager 路由与值班轮转（部署选择，不属代码）。
- [ ] 告警的"静默/抑制"策略（例如发布窗口内抑制 5xx）。

## 9. 补记（2026-11-03，全量盘点时发现的两处计数不一致）

- 本文 §1 写的"6 条告警规则"是起草时的计划数；落地时多了一条 `SettlementLatencyDegraded`
  （结算 p99 退化），**实际 7 条**，见 `deploy/prometheus/hape.rules.yml` 与 §6 的漂移测试。
  保留原文不改，因为它在当时是准的。
- `docs/slo.md` 第 4 条 SLI 仍缺数据源：`backend_allocations_failed_total` 这个 counter 不存在，
  分配成功率算不出来。已登记到 `docs/roadmap.md` §2（判据：Allocate 失败时 +1）。