# 0003 - Store 接口加 context

- 状态：implemented
- 日期：2026-01-01
- 关联 ADR：0006（Postgres 持久化，本条是它的已知债务之一）
- 涉及：7 个 `services/*` 包的 `Store` 接口、14 个实现、全部调用点

## 1. 背景

ADR 0006 记的第一条债务：**`Store` 方法没有 `context`**。SQL 实现只能自己
`context.WithTimeout(context.Background(), 3s)`，后果有两个：

1. **请求取消传不下去**。客户端断开后，数据库查询继续跑到超时，白占一个连接。
2. **超时与请求无关**。3 秒是拍脑袋的常量，而不是调用方给的时间预算。

内存实现看不出问题（没有 IO），所以这条债是被 SQL 实现逼出来的。

## 2. 目标 / 非目标

目标：

1. 每个 `Store` 方法的第一个参数是 `ctx context.Context`。
2. 服务层把 `r.Context()`（HTTP 请求）或调用方传入的 ctx 一路传下去。
3. SQL 实现用调用方的 ctx 派生超时：`context.WithTimeout(ctx, 3s)`——两者取更早的那个。
4. 内存实现接受 ctx 并忽略（`_ context.Context`），保持接口一致。

非目标：

- 不改任何对外契约（21 个 op 的路径、字段、认证、幂等声明全部不动）。
- 不改业务行为、错误码、状态机。
- 不顺手给 `rank.RatingOf` / `matchmaking.FindWaitingByPlayer` 加 error 返回
  （那是 ADR 0006 的另一条债，混在一起会让这次改动无法用"行为不变"来验证）。

## 3. 签名变化（摘要）

| 包 | 之前 | 之后 |
|---|---|---|
| account | `FindOrCreate(externalID, displayName)` | `FindOrCreate(ctx, externalID, displayName)` |
| team | `Join(teamID, playerID)` | `Join(ctx, teamID, playerID)` |
| matchmaking | `Pop(queue, count, band, matchID)` | `Pop(ctx, queue, count, band, matchID)` |
| session | `Update(id, mutate)` | `Update(ctx, id, mutate)` |
| session | `ExpireStale(before)` | `ExpireStale(ctx, before)` |
| session | `RecordResult(match)` | `RecordResult(ctx, match)` |
| progress | `GrantXP(playerID, key, xp, reason)` | `GrantXP(ctx, playerID, key, xp, reason)` |
| economy | `Spend(playerID, key, currency, amount, itemID)` | `Spend(ctx, playerID, key, currency, amount, itemID)` |
| rank | `Apply(board, playerID, key, won)` | `Apply(ctx, board, playerID, key, won)` |

连带变化：

- `matchmaking.RatingLookup` 从 `func(playerID string) int64` 变成
  `func(ctx context.Context, playerID string) int64`——它最终打到 `rank.RatingOf`。
- 三个服务级域方法（`progress.Grant`、`economy.Grant`、`rank.Apply`、`rank.Rating`）
  也加了 ctx，因为它们由 `platform/sink.go` 在结算扇出里调用，而扇出本身有 ctx。
- `session.Expirer` / `session.ResultRecorder` 两个能力接口同步加 ctx。

## 4. 不变量

- 内存实现的行为完全不变（ctx 被忽略）。
- SQL 实现的有效超时 = min(调用方 deadline, 3s)。
- 已取消的 ctx 必须让 SQL 调用立刻返回 `context.Canceled`，而不是等 3 秒。

## 5. 验证

| 层 | 用例 |
|---|---|
| 编译 | 7 个包的接口与两个实现全量改签名，`go build ./...` / `go vet ./...` |
| 等价性 | 七个 `store_equiv_test.go` 在真库上仍逐个 PASS（行为不变的证据） |
| 取消传播 | `TestPGRespectsCancelledContext`：已取消的 ctx 下 `Wallet` / `Grant` 立刻返回 `context.Canceled` |
| 端到端 | 无库 `go test ./...` 全绿；有库 `go test ./services/...` 全绿 |

## 6. 顺带修掉的两个 id bug

验证阶段用真库连跑两次 `platform + fakeds`，暴露出持久的 id 生成问题：

1. `session` 的 id 是 `s_<进程内计数>`，重启后从 `s_1` 重来 → 撞主键。
2. `matchmaking` 的 `match_id` 是 `m_<queue>_<进程内计数>`，重启后重复 →
   `sessions_one_per_match` 把新对局当成重复（409）。

两者都改成时间基（`s_<base36 纳秒>` / `m_<queue>_<base36 纳秒>`）。这不是本次重构
引入的问题，而是**持久化把它暴露出来**：内存里计数器够用，落库后就不再是唯一标识。

## 7. 回滚

纯签名改动，无数据变更。回滚 = revert 提交；数据库与 schema 不受影响。

## 8. 未决

- [ ] `rank.RatingOf` / `matchmaking.FindWaitingByPlayer` 的 error 返回（ADR 0006 另一条债）
- [ ] 3 秒超时是否该由配置给出（挂上真实负载再定）