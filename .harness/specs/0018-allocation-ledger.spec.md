# 0018 - 分配台账（对账的我方一半）

- 状态：implemented
- 日期：2026-11-03
- 关联：spec 0007（Edgegap 适配器）、0015（回收回读）、0017（分配/清扫竞态）
- 涉及：`services/session`（模型、store、报表）、`platform`（指标）、`tools/allocation-report`、`db/migrations/0004`

## 1. 背景

M2 的"计费对账"一直被标成"缺厂商账单接口"。真跑之后发现缺的是**我方的时间线**：

- 会话表记了 `allocation_id`，但没记"什么时候拿到实例"、"什么时候确认它没了"；
- 于是无法回答"这台实例确认释放了吗、跑了多少秒"；
- 也无法区分"刚分配、还没来得及释放"与"分配很久了却没释放"——后者才是漏计费；
- 制作人问"一局多少服务器成本"时，只能回答"取决于厂商账单"。

`release_pending` 只覆盖"试过且失败"；**"从来没走到确认那一步"**（进程崩溃、任务丢失）
没有任何记录。这两者合起来才是完整的对账视图。

## 2. 目标 / 非目标

目标：

1. 记录**分配的左边界**（`allocated_at`）与**释放确认的右边界**（`released_at`）。
2. 提供台账视图：每场对局的实例秒数、是否仍未确认、重试次数。
3. 报表工具把两件事直接回答出来：**哪些实例可能还在计费**、**每台跑了多久**。
4. 指标化："已分配但未确认释放"的数量与最老时长。

非目标：

- 不做厂商账单 API 对账（仍需账单数据；本 spec 只把我方一半做完）。
- 不建新表：两列足够，且避免一次迁移 + 一次 join。
- 不做按 provider/区域的成本分摊（数据够了，等真有多种厂商再说）。

## 3. 设计

```text
create → Allocate 返回 → Update(记录 allocation + allocated_at)        ← 左边界
submitResult / ExpireStale / 竞态交还 → Release 成功 → markReleased()   ← 右边界（含清 pending）
RetryReleases 成功 → markReleased()                                    ← 右边界
```

Schema（迁移 0004）：

| 列 / 约束 | 作用 |
|---|---|
| `allocated_at timestamptz` | 分配器返回实例的时刻 |
| `released_at timestamptz` | 厂商**回读确认**停止的时刻（不是"我们发了请求"） |
| `CHECK released_at IS NULL OR (allocated_at IS NOT NULL AND released_at >= allocated_at)` | 没有分配就没有释放；时间不能倒流 |
| `INDEX sessions_unreleased (allocated_at) WHERE allocated_at IS NOT NULL AND released_at IS NULL` | 台账/告警只扫可能还在计费的那几行，正常情况索引为空 |

查询能力（`AllocationLedger`，可选 store 能力，和 `Expirer`/`ReleaseTracker` 同理）：
`Allocations(ctx, AllocationQuery{UnreleasedOnly, Since, Limit})`，按 `allocated_at` 倒序。

报表工具 `tools/allocation-report`：

```bash
BACKEND_DATABASE_URL=postgres://... go run ./tools/allocation-report            # 全部
go run ./tools/allocation-report -unreleased -since 24h                          # 只差未确认的
go run ./tools/allocation-report -json | jq '.[] | select(.unreleased)'          # 给脚本用
```

它直接用与进程相同的 store/service，**不另写一套查询**，所以报表口径不会与线上行为漂移。
输出：

```text
SESSION          PROVIDER  ALLOCATION          STATE     ALLOCATED       RELEASED        SECONDS  RETRY
s_dmlcbaxn2vr0   local     local-s_...-1       finished  11-03 05:52:13  11-03 05:52:14  1        -

1 allocations, 0 unreleased, 1s of instance time
```

指标（reconciler 每轮发布）：

| 指标 | 含义 |
|---|---|
| `backend_unreleased_allocations` | 有分配但无确认释放的会话数（唯一可能仍在计费的行） |
| `backend_unreleased_oldest_seconds` | 其中最早的那台已经漂了多久 |

## 4. 失败模式

| 场景 | 行为 |
|---|---|
| 时间戳写入失败 | 打 ERROR 但**不影响**释放/结算（实例已经没了，记账失败不能反噬业务） |
| `released_at` 早于 `allocated_at` | schema 拒绝 |
| 有 `released_at` 但无 `allocated_at` | schema 拒绝 |
| 分配成功但进程崩溃，从未释放 | 台账里是 `unreleased`，不等同于 `release_pending`（后者是"试过但失败"） |
| 本地 allocator（无真实实例） | 同样记录，成本为 0，报表仍可用 |

## 5. 验证

| 层 | 用例 |
|---|---|
| 迁移 | 0004 在 PG 16.4 上应用成功；`db-migrate.sh` 重跑为 no-op |
| 约束断言 | `0004-allocation-ledger.probe.sql` 7 条：无分配也可建会话、记录分配、释放晚于分配、释放早于分配被拒、无分配就释放被拒、从未分配却释放被拒 |
| 存储等价性 | 两列在内存/SQL 往返一致；`Allocations` 的未确认过滤、`since` 下界、`limit` 在两个实现上一致 |
| 服务 | 结算释放后台账一行：`ReleasedAt` 非空、`Unreleased=false`、实例秒数 ≥ 0；释放失败时 `Unreleased=true`、`ReleasePending=true`、秒数约等于经过时间 |
| 平台 | reconciler 一轮后 `backend_unreleased_allocations=1`；厂商恢复后变 0 |
| 实跑 | 真库 + 本地 allocator 跑完一局：报表显示 `finished`、1s 实例时长、0 unreleased；`-unreleased` 为空；`-json` 可解析 |

## 6. 回滚

代码回退即恢复"只有 `release_pending`"的视图；两列留着无害（可空）。

## 7. 未决

- [ ] **厂商账单对账**（本 spec 的另一半）：把 `/v1/deployments` 或账单导出的每台时长与本台账逐行比对。触发：首次真实计费对账。
- [ ] 成本模型：按 provider/区域的单价把 `instance_seconds` 换算成钱（需要厂商价目）。
- [ ] 竞态（spec 0017）发生次数没有独立指标，只有 WARN 日志。
- [ ] 台账清理策略：长期保留会单调增长，可按月归档到冷表。
## 补记（2026-11-03）

本文遗留的"台账清理策略"已由 spec 0020 落地：迁移 0005 建 `*_archive` 冷表，
`scripts/db-archive.sh` 搬走"已结束且已确认释放、早于 `ARCHIVE_BEFORE`"的会话（dry-run 默认，
确认才写，重跑 no-op）。成本模型（单价换算成钱）仍缺厂商价目，见 §未决。
