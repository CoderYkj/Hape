# 0017 - 分配与超时清扫的竞态

- 状态：implemented
- 日期：2026-11-03
- 关联：spec 0003（会话状态机）、0007（Edgegap 适配器）、0015（回收回读）
- 涉及：`services/session`（`create`）、`platform`（启动告警）、`pkg/battlealloc`（导出默认就绪超时）

## 1. 背景

托管厂商的分配**不是秒级**：Edgegap 实测 4–7s，文档口径 20–90s，适配器的就绪超时是 3 分钟。
而"僵尸会话清扫"（`session.ExpireStale`）会按 `BACKEND_SESSION_STALE_AFTER`（默认 5 分钟）
把仍处于 `allocating` 的会话置为 `failed` 并释放其分配。

问题：如果 `SESSION_STALE_AFTER` 被设为**小于分配耗时**（例如演练时设 3 秒），清扫会在
`Allocate` 还在等待时就把会话关掉。实测现象（真厂商，2026-11-03）：

```text
session=s_xxx state=failed provider=edgegap address=172.237.14.252:31264  分配耗时=7s
WARN release after expiry failed ... allocation_id="" err="allocation id is required"
INFO pending release confirmed ... allocation_id=773929dbd91b attempts=6
```

即：`create` 返回了 `state=failed` 的会话，但它**握着一台正在运行的实例**；第一次释放尝试
用的是清扫快照里为空的 `allocation_id`，靠后来 5 次重试才把实例交还。最终状态是对的
（厂商侧 deployments 归零），但中间有三处不该发生：

1. `create` 把"分配成功但会话已关闭"当成成功返回（200 + failed）；
2. 释放靠"重试循环兜底"而不是在发现竞态时立即执行；
3. 空 `allocation_id` 的释放尝试是纯噪音（每次都是永久错误）。

## 2. 目标 / 非目标

目标：

1. `create` 在拿到分配后**再确认会话仍然存活**；已被清扫则立即交还实例并返回 503。
2. 交还失败时把分配记录在会话上并标 `release_pending`，交给 reconciler 重试（不丢）。
3. 配置层面给出预警：`SESSION_STALE_AFTER` 不高于分配器就绪超时时，启动打 WARN。

非目标：

- 不改清扫策略本身（`in_progress` 仍不扫：杀一局正在打的对局比漏一台更糟）。
- 不引入分布式锁：竞态检测在数据库的一次 `Update` 上完成，天然原子。

## 3. 设计

```text
create:
  mint token → 落库(state=allocating) → Allocate(...)          ← 这里可能耗时数分钟
                                        ↓ 返回
  Update(记录 allocation_id/provider/address/port)  ← 一次加锁写，顺带拿到"当前状态"
                                        ↓
  state == allocating ? ── 是 ──→ 200（正常路径）
                        └─ 否 ──→ Release(allocation_id)
                                    ├─ 成功 → 503 unavailable "会话在服务器启动期间被关闭"
                                    └─ 失败 → markReleasePending（attempts+1）+ 同一个 503
```

为什么用一次 `Update` 而不是先 `Get` 再判断：`Update` 在 store 内加锁读改写，判断与记录分配
是同一个原子步骤；分成两步会重新引入窗口。

为什么先把分配记上再释放：释放可能失败。如果没有 `allocation_id` 落在会话上，重试循环
就无从下手（上面日志里那个空 id 的死循环就是反例）。

## 4. 失败模式

| 场景 | 结果 |
|---|---|
| 分配期间会话被清扫 | `create` 503 `unavailable`（带 `session_id` / `session_state`），实例已交还 |
| 交还也失败 | 同上 503；会话 `release_pending=true`，reconciler 重试；attempts 递增 |
| 正常分配 | 200，state=allocating（行为不变） |
| `Allocate` 本身失败 | 503，会话置 failed（行为不变，spec 0003） |
| `STALE_AFTER` ≤ 就绪超时 | 启动 WARN（不阻断：竞态已被处理，只是会变成 503） |

## 5. 验证

| 层 | 用例 |
|---|---|
| 服务单测 | `TestAllocationRacingTheStaleSweepIsReleased`：分配过程中把会话置 failed → 503、`Release` 恰好一次、分配记录留在会话上、无 pending |
| 服务单测 | `TestRaceWithAnUnconfirmedReleaseStaysRetryable`：交还失败 → 503、`release_pending=true`、attempts=1 |
| 真机 | 后端直连 Edgegap：`STALE_AFTER=3s`（小于 7s 分配）复现原缺陷；修复后同样配置下 `create` 返回 503 且实例立即交还（厂商 deployments 归零） |
| 真机（正常配置，2026-11-03） | `STALE_AFTER=20s`、分配实测 **4s**：会话被扫成 failed → 首次释放即回读确认（日志 `expired session's server released`）→ 厂商 `/v1/deployments` **total_count=0**；`pending/max_attempts/oldest_pending` 全为 0（计数器只统计**重试**，首次成功不入计数，这是刻意的） |
| 配置 | 启动日志在 `STALE_AFTER` ≤ 3m 时出现 WARN（`platform` 单测 / 人工） |

## 6. 回滚

代码回退即恢复"分配完成后无条件记录并返回"，竞态回到"靠回收重试兜底"。两列（`release_pending`
/ `release_attempts`）与迁移不受影响。

## 7. 未决

- [ ] 清扫与"分配中"的更显式协调（例如 `allocation_started_at` + 清扫跳过仍在就绪超时内的行）：
      现在的做法是"事后检测 + 交还"，够用且不需要新状态；如将来出现更长分配的厂商，再考虑前置判断。
- [ ] 竞态发生次数没有独立指标（目前只有 WARN 日志 + `release_failed_total`）。
## 补记（2026-11-03）

本文遗留的"竞态发生次数没有独立指标"已由 spec 0020 补上：
`backend_session_sweep_races_total`（`services/session` 在"分配完成时会话已被清扫关闭"处 +1）。
