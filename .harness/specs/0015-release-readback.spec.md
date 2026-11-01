# 0015 - 回收回读与重试（不再相信 `DELETE` 的 200）

- 状态：implemented
- 日期：2026-11-01
- 关联 ADR：0003（分配抽象）、0007（结算原子性）；前置 spec 0007（Edgegap 适配器）
- 涉及：`pkg/battlealloc`（`Edgegap.Release`）、`services/session`（重试路径）、`platform`（reconciler）、`db/migrations/0003`

## 1. 背景

`Release` 之前只发 `DELETE /v1/stop/{id}`：2xx 或 404 就当成功。但厂商的 200 只表示
**"接受了停止请求"**，实例可能还在跑、还在计费。两个调用点（`submitResult` 结算后、
`ExpireStale` 超时回收）都是 best-effort、失败只打一行 warn——所以"没释放成功"对系统
**不可见**：它只在账单上可见。仓库里唯一一次确认是人工 `GET /status` 看到 `TERMINATED`
再核对 deployments 归零。

推论：一个只会发请求、不会确认的回收实现，等价于"我们希望它停了"。

## 2. 目标 / 非目标

目标：

1. 回收 = 停止请求 **+ 回读确认**（`running == false` 或 404 才算成功）。
2. 回读是有预算的（默认 30s，1s 轮询），不会挂住调用方。
3. 确认失败变成**可重试的记录**，由 reconciler 每一轮重试；次数是唯一的告警信号。
4. 回读失败**绝不**让玩家的结算失败。

非目标：

- 不做跨机器的分布式锁：单进程 reconciler，`RetryReleases` 幂等，多实例时会重复释放
  （对 `Release` 来说是安全的，只是多几次 API 调用）。
- 不做计费对账/成本看板（需要厂商账单 API）。
- 不改 `Allocator` 接口形状。

## 3. 设计

```text
Release(ctx, allocationID)
 ├─ stop(): DELETE /v1/stop/{id}
 │    2xx / 404 → 继续回读          429/5xx → ErrRetryable   401/403 → ErrPermanent
 └─ waitStopped(): GET /v1/status/{id} 循环（StopTimeout=30s, StopPollInterval=1s）
      404            → 成功（厂商已忘记它）
      running=false  → 成功
      running=true   → 继续等，超预算 → ErrRetryable + 最后一次 current_status
      读取 429/5xx    → 继续等（瞬时错误不该放弃一台很可能还在跑的机器）
      读取 401/403    → ErrPermanent（凭证问题，重试无用）
```

`Allocate` 内部放弃一条部署时用 `stop()`（只发请求、不等确认）：那时还没有 allocation id
被任何会话记住，没人能重试它，所以不要为一个注定无人托管的部署等 30 秒。

**重试路径**（`db/migrations/0003`）：

| 列 | 作用 |
|---|---|
| `release_pending boolean not null default false` | 回收尚未被确认 |
| `release_attempts integer not null default 0` | 失败次数（唯一的漏计费告警信号） |
| 约束 `sessions_pending_release_has_allocation` | 待回收必须有 `allocation_id`：没有可释放的对象，置位只能是 bug |
| 部分索引 `WHERE release_pending` | 清扫只查这几行，正常情况下索引是空的 |

流程：

```text
submitResult / ExpireStale
   Release 失败 → markReleasePending（+1 attempt，只记日志，不影响响应）
                          ↓
reconciler tick：先 ExpireStale，再 RetryReleases（每轮最多 25 条）
   Release 成功 → 清 pending，日志 attempts=N 已确认
   Release 失败 → attempts+1，保持 pending，warn 带 attempts
```

`ReleaseTracker` 是**可选**的 store 能力（和 `Expirer` 同理）：不能扫描的 store 不实现它，
调用方报 `ErrNoReleaseTracker` 而不是假装扫过了。

## 4. 失败模式

| 场景 | 结果 |
|---|---|
| 停止请求被接受、实例立刻消失 | 成功（回读一次） |
| 停止请求被接受、实例仍在跑 | 30s 后 `ErrRetryable`，错误含 provider 状态 → 记录并重试 |
| 停止请求 429/5xx | `ErrRetryable`（没发出去，下一次重试） |
| 停止请求 401/403 | `ErrPermanent`（告警，重试无用） |
| 回读 404 | 成功 |
| 回读 401/403 | `ErrPermanent`（等下去不会变） |
| 回读 5xx 中途恢复 | 继续等到成功（瞬时错误被吸收） |
| 结算时回收失败 | 战报**仍然** 200 `applied=true`，只是多一条待回收记录 |

## 5. 验证

| 层 | 用例 |
|---|---|
| 适配器（假厂商） | 停止后两次 `running=true` 再 `false` → 成功且读了 3 次状态；一直 `running=true` → `ErrRetryable` 且错误含状态与"still running"；回读 404 → 成功；回读 500 后恢复 → 成功；回读 401 → `ErrPermanent`；原有用例（幂等 404、stop 500/401）保持 |
| 服务 | 回收失败 → 战报仍 `applied=true`，会话 `release_pending=true`、`release_attempts=1`；allocator 恢复 → `RetryReleases` 清标记并返回 1；持续失败 → attempts 递增且保持 pending |
| 存储等价性 | 两列在内存与 SQL 中同样往返；`PendingReleases` 在两个 store 里都能找到待回收行 |
| schema 探针 | `0003_release_pending.probe.sql` 6 条：默认 false/0、置位允许、无 allocation 置位被拒、负数 attempts 被拒、清标记允许 |
| 迁移 | 本机 PG 16.4 上 `db-migrate.sh` 应用 0003 并记录校验和，重复执行为 no-op |

## 6. 回滚

代码回退即恢复"只发停止请求"。两列留着无害（默认 false/0）；`RetryReleases` 不再被调用
时，未确认的释放就回到"只在日志里"的旧行为。

## 7. 未决

- [x] **真机复验（2026-11-01，两次）**：`tools/edgegap-check`（走生产适配器 + 并行状态采样）
  在真实 Edgegap 上实测：
  - 分配 → `READY`：**4s**（两次一致），地址形如 `172.237.14.252:32511`；
  - 释放 → `running=false`：**2.0s / 0.7s**（采样 1s / 300ms）；
  - 状态序列：`Status.READY -> Status.TERMINATING`，且 **`running` 在 `current_status`
    仍是 `Status.TERMINATING` 时就变 false**——所以判定必须看 `running`，不能等字符串
    `TERMINATED`（本 spec §3 的实现正是如此，这次实测确认它不是碰巧对）；
  - 释放后 `/v1/deployments` 的 `total_count` 归零，没有残留。
  - 仍未覆盖：`Status.TERMINATING` 窗口内是否仍有秒级计费。我们确认的是"不在运行"，
    不是"不再计费"；账单对账仍在 §9 的未决里。
- [ ] **告警**：`release_attempts` 目前只有日志。接指标后应有一条
  "同一 session 的 attempts 持续增长" 的规则（spec 0014 §9 也提到缺告警指标）。
- [ ] 多实例 reconciler 的单活选主（现在多实例会重复调用 Release，安全但浪费配额）。
- [ ] 厂商账单对账（成本看板），本 spec 只解决"实例没停"。