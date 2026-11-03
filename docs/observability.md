# 观测与运维（指标 / 告警 / 台账 / 基线）

> 读者：负责上线后运维与容量的人 · 状态：活文档（2026-11-03） · 上游：spec 0015 / 0018 / 0019
>
> 部署步骤在 `docs/deploy.md`；SLO 目标在 `docs/slo.md`。本文只讲**上线之后怎么看、怎么查**。

这三节原本是部署手册的 11.8–11.10。它们回答的问题和"怎么把服务部署起来"不是一类，
所以独立成篇：部署是一次性动作，观测是每天的动作。
## 1. 回收回读的指标与告警

回收失败不是可见的 5xx：它是一条待重试记录（`sessions.release_pending`）。所以它必须
变成数字，否则"服务器没停、还在计费"只会在账单上出现。

`/metrics` 新增（spec 0015）：

| 指标 | 类型 | 含义 |
|---|---|---|
| `backend_release_pending_sessions` | gauge | 当前有多少场会话的回收尚未被确认（健康时恒为 0） |
| `backend_release_max_attempts` | gauge | 待回收里最严重那场的重试次数（持续增长 = 厂商不放） |
| `backend_release_oldest_pending_seconds` | gauge | 最老的待回收等了多久（从会话结束算，没结束的从创建算，是上界） |
| `backend_release_failed_total` | counter | 累计失败的回收重试次数 |
| `backend_release_confirmed_total` | counter | 累计成功的回收重试次数 |
| `backend_allocations_failed_total` | counter | 分配失败次数（`slo.md` 第 4 条 SLI 的分子一半，spec 0020） |
| `backend_session_sweep_races_total` | counter | 分配完成时会话已被清扫关闭的次数（spec 0017 遗留的那个数） |
| `backend_rate_limited_{player,service,public}_total` | counter | 各类调用方被 429 的次数（spec 0020） |

指标由 reconciler 每一轮发布（`Platform.ReconcileOnce`），所以**必须开启**
`BACKEND_RECONCILE_INTERVAL`（生产 compose 默认 30s）。没开时这些序列不存在，告警不会
误报，但也永远不会响——这本身是要在部署检查单里确认的一项。

告警规则在 `deploy/prometheus/hape.rules.yml`（一条）：

```yaml
- alert: DedicatedServerReleaseStuck
  expr: backend_release_pending_sessions > 0
  for: 15m
```

为什么 15 分钟：厂商通常几秒到几十秒完成销毁，30 次重试仍不消失基本不是"慢"，而是
"卡住"了。告警后按顺序处理：

1. `backend_release_max_attempts` — 多少次了；
2. 后端日志 grep `release retry failed`，拿 `allocation_id`；
3. 去厂商控制台查这个 deployment 的真实状态；必要时手工 `DELETE /v1/stop/{id}`；
4. 若厂商侧已终止而本地仍 pending，先看 `current_status` 的具体取值。实测（2026-11-01）
   真实取值带 `Status.` 前缀：`Status.READY -> Status.TERMINATING`，且 `running` 在状态
   仍是 `TERMINATING` 时就变 false——判定必须看 `running`，等字符串 `TERMINATED` 会多一个
   超时竞态。

验证方式（不需要真的厂商）：单测 `TestReconcileOncePublishesTheReleaseBacklog` 用一个
"永远不释放"的假分配器跑完整链路——战报仍 200、会话进 pending、gauge 变 1、恢复后 gauge
回 0 且 `confirmed_total` +1。

## 2. 分配台账（对账用）

每次分配都会在会话上留下两个时间点：`allocated_at`（拿到实例）与 `released_at`（厂商**回读确认**
实例已停止）。由此可以回答两个运营问题，而不必等厂商账单：

- **哪些实例可能还在计费**：有 `allocated_at`、没有 `released_at` 的行。注意它与
  `release_pending` 不是一回事：后者是"试过、失败了"，前者还包括"从来没走到确认那一步"
  （进程崩溃、任务丢失）。
- **每台实例跑了多久**：`released_at - allocated_at`。这是每场对局服务器成本的口径。

```bash
# 全部台账（最近的在最上面）
BACKEND_DATABASE_URL=postgres://... go run ./tools/allocation-report

# 只差"可能还在计费"的行
go run ./tools/allocation-report -unreleased -since 24h

# 给脚本/看板用
go run ./tools/allocation-report -json | jq '.[] | select(.unreleased)'
```

报告末尾会汇总 `N allocations, M unreleased, Xs of instance time`。工具直接用进程相同的
store/service，所以口径不会与线上漂移。

指标（reconciler 每轮发布，需要开启 `BACKEND_RECONCILE_INTERVAL`）：

| 指标 | 含义 | 建议用法 |
|---|---|---|
| `backend_unreleased_allocations` | 有分配、无确认释放的会话数 | 告警候选：`> 0` 持续 15 分钟；正常应为 0 |
| `backend_unreleased_oldest_seconds` | 最老那台漂了多久 | 判断是"刚分配"还是"真卡住" |

与第 1 节的关系：第 1 节的 `release_pending_*` 衡量**重试积压**；本节的 `unreleased_*` 衡量
**从未确认**。两者都为 0 才说明没有逃逸的实例。

## 3. 观测接入与容量基线

**接线**（可选 profile，不影响后端启动）：

```bash
docker compose -f deploy/docker-compose.prod.yml --profile observability up -d prometheus
curl -s http://127.0.0.1:9090/api/v1/rules | jq '.data.groups[].rules[].name'
```

规则文件 `deploy/prometheus/hape.rules.yml` 共 6 条（spec 0019）：

| 告警 | 看什么 | 严重级 |
|---|---|---|
| `DedicatedServerReleaseStuck` | 回收重试积压 | warning |
| `UnreleasedAllocation` | 有分配、无确认释放（可能仍在计费） | warning |
| `SessionsStuckAllocating` | 分配超过 3 分钟 | warning |
| `ZombieSession` | ready/in_progress 超过 30 分钟 | warning |
| `HighServerErrorRatio` | 5xx 比例 > 1%（5 分钟） | critical |
| `SettlementLatencyDegraded` | 结算 p99 超过 0.3s | warning |
| `ServiceScopeDenied` | 某凭证在 10 分钟内出现越权（强制）或 would-deny（观察） | warning |
| `EconomyInternalError` | 钱包/账本操作出现 500 | critical |

前置条件：**必须开启 `BACKEND_RECONCILE_INTERVAL`**（生产 compose 默认 30s）。没开时业务 gauge 不存在，
告警不会误报，但也永远不会响——这一条要进部署检查单。

CI 会跑 `promtool check rules` 与 `promtool check config`；另有一条测试断言"规则里引用的每个指标
都真的被导出"（否则告警是假覆盖）。

**压测基线**（本机、单进程、dev auth、本地 allocator、loopback）：

```bash
go run ./tools/loadtest -matches 400 -concurrency 25            # 内存 store
BACKEND_DATABASE_URL=... go run ./tools/loadtest -matches 200 -concurrency 10   # Postgres
```

| 场景 | 吞吐 | total p50 | total p99 |
|---|---|---|---|
| 内存 store（并发 25） | 307 场/秒 | 80ms | 95ms |
| Postgres store（并发 10） | 121 场/秒 | 59ms | 537ms |

怎么读：两行并发不同，p50 不可横比；Postgres 的尾巴来自连接池与行锁竞争；
这些数字是**本机形态的天花板量级**，不是生产容量承诺。真实容量需要多机 + 真实网络 + soak。