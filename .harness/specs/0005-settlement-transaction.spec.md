# 0005 - 结算单事务（ADR 0007 选项 2）

- 状态：implemented
- 日期：2026-01-01
- 关联 ADR：0007（决策）、0006（原债务）
- 涉及：`platform/sink.go`、`pkg/pg`、`services/{economy,rank,progress}/store_pg.go`

## 1. 背景

一次战报结算要写四个地方：战报存档、进度、经济、排行，最后把会话置 finished。
之前是五步独立写入，任何一步失败都留下"半截账"，重试能收敛但中间状态对外可见。
ADR 0007 决定：数据库模式下把整段放进一个事务。

## 2. 目标 / 非目标

目标：

1. 数据库模式：战报 + 三域 + 会话状态**一起提交或一起回滚**。
2. 内存模式：保持原样（无法跨 store 事务），行为是幂等收敛。
3. 幂等语义不变：同一个 `result_id` 重放不重复发放。
4. 失败语义不变：任何一步失败 → 503 → 同一 `result_id` 重试。

非目标：

- 不引入 outbox / 消息队列（那是 M4 拆进程时的事）。
- 不改契约、不改奖励数值、不改错误码。
- 不为内存模式模拟事务。

## 3. 实现要点

| 位置 | 改动 |
|---|---|
| `pkg/pg` | 新增 `RunInTx(ctx, pool, db, fn)`：有池自己开事务，无池（已在事务里）直接复用 |
| 三个 store | 新增 `NewWithDB(db pg.DB)`；内部事务段改用 `RunInTx` |
| `economy.movement` | 幂等检测改为 `INSERT ... ON CONFLICT DO NOTHING` + `RowsAffected`，不再靠捕获唯一键异常（异常会 abort 整个外层事务） |
| `platform/sink.go` | 数据库模式开一个事务，包装三个事务作用域 service 后跑奖励规则；内存模式走原路径 |
| 日志 | 区分 `match fan-out complete` 与 `... (memory stores: idempotent, not atomic)` |

## 4. 不变量

- 事务内任一步失败 → 全回滚 → 会话停在 `ready`/`in_progress` → 503。
- 同一 `result_id` 重放：账本 / 经验 / MMR 各只有一条。
- 事务里不调用任何外部服务（HTTP、厂商 API）；`90-battle-integration.md` 已加粗这条。

## 5. 验证

| 层 | 用例 |
|---|---|
| 原子性 | `platform/sink_test.go::TestSettlementIsAtomic`：名单里放一个不存在的玩家 → 失败；断言合法玩家的六张表计数全 0 |
| 成功路径 | 同一测试去掉幽灵玩家 → 三域各 1 行 |
| 幂等 | 同一测试重放 → 账本仍 1 行 |
| 回归 | 七个等价性测试、e2e、平台测试全绿（含真库两轮） |
| 活体 | `platform + fakeds` 真库跑通，日志只出现一次 `match fan-out complete`，三域各 2 行 |

## 6. 回滚

纯代码改动，无 schema 变更。回滚 = revert；数据层面不需要迁移。

## 7. 未决

- [ ] 内存模式是否也值得模拟事务（现在靠幂等收敛；只有测试会用到内存模式）
- [ ] M4 拆进程时改用 outbox（ADR 0007 已写死，不是"可以考虑"）
- [ ] 事务内是否要加超时上限（现在继承请求 ctx；`sink` 没有自己的预算）