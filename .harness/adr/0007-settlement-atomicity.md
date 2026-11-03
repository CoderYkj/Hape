# ADR 0007 - 结算扇出的原子性

- 状态：**accepted**（2026-01-01 拍板：选项 2，单事务）
- 已实现：`platform/sink.go` + 三个 PG store 的 `NewWithDB`（见 spec 0005）
- 日期：2026-01-01
- 关联：ADR 0006（第 1 条剩余债务）、`platform/sink.go`、`services/session/service.go`
- 决策点：一次战斗结算要写三个域（进度、经济、排行），这三笔写**要不要原子**

## 背景：现在发生什么

`session.submitResult` 的顺序是：

```text
1. 存档战报（match_results / match_player_results，幂等 by result_id）
2. 扇出：progress.Grant → economy.Grant → rank.Apply（各自幂等，各自一个事务）
3. 把 session 置 finished（写 sessions.result_id）
```

任何一个域失败，第 3 步不执行：session 停在 `ready`/`in_progress`，返回 503，
调用方用**同一个 `result_id`** 重试。因为三步的幂等键都从 `result_id` 派生
（`<result_id>:<player>:xp|gold|mmr`），重试是安全的，最终收敛。

### 它现在不满足什么

设玩家 A、B 打了一场，A 的写入成功、B 的写入失败（数据库抖动、配置错误、
某条记录不合法）。在一次失败的请求之后，外界能看到：

```text
A: gold 100, xp 140, rating 1024     ← 已发
B: gold 0,   xp 0,   rating 1000     ← 未发
session.state = ready（未完成）
```

也就是说：**奖励可以部分可见**。账本是追加的、幂等的，所以重试会补齐，但
"补齐之前"这个窗口是真实存在的，而且：

- 玩家看得到金币、看不到经验（或反之），客服无法给出统一解释。
- 排行榜可能已经把 A 的 1024 分算进去，匹配却仍按 session 未结束处理。
- 如果重试永远不来（客户端放弃、调度器没接上），这笔账就停在半途——
  目前**没有对账任务**发现它（`platform.RunReconciler` 只清理超时会话，
  不会补发奖励）。

## 选项

### 选项 1：保持现状，但把它变成"有文档、有监控"的最终一致

做法：

- 不变事务边界。
- 给"扇出失败"加指标与告警（现在只有一行 error 日志）。
- 增加一个对账任务：扫"已存档战报但会话未 `finished`"的组合——注意 `sessions.result_id`
  是在最后一步才写的，所以对账要用 `match_results` 反查：

  ```sql
  SELECT r.result_id, r.session_id
    FROM match_results r
    JOIN sessions s ON s.id = r.session_id
   WHERE s.state <> 'finished';
  ```

  命中即重放扇出（幂等键已经在表里，重放安全）。

代价：实现最小（约 100 行 + 一个指标）；部分可见窗口依然存在。
风险：对账任务本身没被验证过就会成为新的静默点。
适合：如果"窗口内部分可见"被认为可接受（社交游戏常见）。

### 选项 2：单事务（推荐）

做法：

- `sink.MatchFinished` 开一个 `pg.InTx`，把三个域的写入放进同一个事务。
- 三个 PG store 增加一个"绑定到 `pg.DB`（池或事务）"的构造：
  `economy.NewPG(pool)` 与 `economy.NewWithDB(db)`；语句层面它们本来就只依赖
  `pg.DB` 接口，改动集中在"谁开事务"。
- 顺序变成：`begin → 存档 → 三个域写入 → session 置 finished → commit`。
  任一步失败：全部回滚，session 不动，503 重试。

优点：一次性消除部分可见窗口；业务上"奖励 + 会话状态 + 战报"同时成立与否。
代价与限制：

- 三个域被同一个事务绑在一起，**这本身就是耦合**。将来 M4 拆进程时，
  这个事务必须换成 outbox（选项 3），否则拆不动。
- 内存实现无法给跨 store 事务（它们是各自独立的 map），所以**内存模式下
  仍然是最终一致**。等价性测试因此不能断言原子性，只能断言"幂等收敛"。
- 事务变长（三域写入 + 战报），持有锁的时间比现在长；一个玩家的慢写入会拖住
  所有人的同一场结算。按当前数据量可忽略，但要写进已知限制。

### 选项 3：Outbox + 投影

做法：

- `submitResult` 在**同一个事务**里写一条 `match_finished` 事件（落库），
  并把 session 置 finished。这一步是原子的、也是唯一的事务写。
- 一个投影 worker 读事件、调三个域、标记 `projected_at`；失败重试，
  幂等键与现在一致。

优点：写路径原子且短；投影可重放、可观测、可积压；**M4 拆进程后天然可用**
（事件表就是消息队列的替身）。
代价：多一张表、一个 worker、一套游标/重试/积压监控。对一个还没有真实流量的
项目，这是提前支付复杂度；但它是唯一在拆分后仍然正确的方案。

### 选项 4：Saga + 补偿

做法：每步失败就回滚已完成的步（退钱、扣经验、扣分）。

**不采纳**：补偿本身就是一次写，也可能失败；用"再写一笔反向流水"来修
"写失败"是拿不确定性治不确定性。资金路径上这条尤其危险。

## 比较

| | 1 现状+对账 | 2 单事务 | 3 Outbox | 4 Saga |
|---|---|---|---|---|
| 部分可见窗口 | 存在 | **消除** | 消除（投影前不可见） | 存在（补偿期） |
| 实现量 | 小 | 中 | 中大 | 大 |
| 拆分后是否成立（M4） | 成立 | **不成立** | **成立** | 成立 |
| 新增静默失败点 | 对账任务 | 无 | 投影 worker | 补偿逻辑 |
| 与当前代码距离 | 0 | 近（sink + 三个 store 构造） | 远（新表、worker） | 远 |

## 建议

**现在做选项 2，并在 ADR 里写死"M4 拆进程时改用选项 3"**。理由：

1. 现在 7 个域共用一个进程、一个数据库，事务是免费的，可选 2 的代价主要是
   "代码里出现一处跨域事务"，而不是新的基础设施。
2. 部分可见窗口对"奖励"这种玩家会立刻核对的东西是实打实的客服成本。
3. 选项 2 的实现路径（store 绑定 `pg.DB`）与选项 3 的投影 worker 需要的能力
   （"在给定事务里执行"）**是同一块积木**，不会被丢弃。

## 采纳选项 2 时需要一并做的事

1. `platform/sink.go` 改为接收一个"事务作用域的 store 集合"，或在 `MatchFinished`
   内部 `InTx` 并用 `NewWithDB(tx)` 构造三个 store。
2. 三个 PG store 增加 `NewWithDB(db pg.DB)`；内部凡是"自己开事务"的方法
   （`economy.movement`、`rank.Apply`）在已处于事务中时改为直接执行——需要一个
   显式标记，不能靠类型断言猜。
3. 内存实现加注释说明"非原子"，并**不要**在测试里断言原子性；只断言幂等收敛。
4. 失败语义不变：任何一步失败 → 整体回滚 → session 不变 → 503 → 同一
   `result_id` 重试。
5. 已知限制写进 README 与 ADR：事务跨越三个域，拆分时必须换 outbox。

## 实现结果（采纳选项 2 之后）

- `pkg/pg` 增加 `RunInTx(ctx, pool, db, fn)`：有池就自己开事务，没有池（已处在
  事务里）就直接复用。三个 store 因此只有一套代码路径。
- `economy` / `rank` / `progress` 的 PG store 增加 `NewWithDB(db pg.DB)`，内部凡是
  "自己开事务"的地方改用 `RunInTx`。
- `economy.movement` 的幂等检测从"捕获唯一键异常"改成
  `INSERT ... ON CONFLICT DO NOTHING` + `RowsAffected`：捕获异常会 abort 整个事务，
  而单事务结算不能被一次重放拖垮（这条与 ADR 0006 踩坑记录第 2 条一致）。
- `sink.MatchFinished` 在数据库模式下开一个事务，把三个域包装成事务作用域的
  service 后跑完奖励规则，再统一提交；内存模式保持原样（无法跨 store 原子）。
- 日志区分两种结果：`match fan-out complete`（数据库模式）与
  `match fan-out complete (memory stores: idempotent, not atomic)`。

### 验证

`platform/sink_test.go::TestSettlementIsAtomic`（真库）：

1. 一份名单里放一个不存在的玩家 → 扇出失败；
2. 断言**合法玩家那一条也没落地**（六张表计数全 0）——这就是回滚的证据；
3. 去掉幽灵玩家再跑一次 → 三域各 1 行；
4. 用同一个 `result_id` 重放 → 账本仍是 1 行，不重复发钱。

### 遗留

- 内存模式仍是最终一致（各 store 各自持有 map），测试只断言幂等收敛。
- 事务跨越三个域，是**有意的耦合**：M4 拆进程时必须改成选项 3（outbox）。
  ADR 0006 与本文都记了这一条，不允许悄悄拆。

## 验收标准（选项 2）

```text
Given 三个域中任意一个在写入时失败
When  提交战报
Then  sessions 状态不变（ready/in_progress）、无任何领域数据落地、返回 503

Given 同一 result_id 重试成功
Then  三个域各落一次，session 变 finished，无重复入账
```

## 未决（需要你拍板）

> **补记（2026-11-03）：这一节已被上面「实现结果」回答，保留原文是为了不篡改当时的记录。**
>
> 1. 已选 **选项 2**（单事务），见 §「实现结果（采纳选项 2 之后）」与 spec 0005。
> 2. 已接受：内存模式不保证原子，测试只断言幂等——`platform/sink_test.go` 的日志里明确写
>    `match fan-out complete (memory stores: idempotent, not atomic)`。
> 3. 选项 3（outbox）不在本轮，触发条件写死为 **M4 拆进程**；到那时必须改，不允许悄悄拆。

- [x] 选 1 / 2 / 3 中的哪一个？→ 2
- [x] 若选 2：是否接受"内存模式不保证原子"？→ 接受
- [x] 若选 1：是否同时加对账任务？→ 不适用（未选 1）