# 0002 - M1 持久化：七个域落 Postgres

- 状态：implemented（7/7 域已落库；含战报留档）
- 日期：2026-01-01
- 关联 ADR：0006（驱动与策略）、0005（模块化单体）
- 涉及服务：account、economy（本批）；其余五个域仍内存

## 1. 背景

M0 证明了这条链是通的，但所有数据都在内存里：进程一重启，钱包、等级、战绩全没。
`db/migrations/0001_init.sql` 已经写好并在 Postgres 16.4 上验证过约束，缺的是
Go 侧的实现与装配。

第一批：account + economy。理由是它们互为前提——`ledger_entries.player_id` 有外键
指向 `players`，只迁经济不迁账号，发钱会直接失败（这是数据完整性，不是耦合失误）。

第二批：progress + rank。它们同属"结算扇出"的三个目标域之一，且 rank 的
`rating_events` 天然带幂等主键；progress 需要一个新表（`0002_progress_grants.sql`）
承载幂等键，因为 `player_progress` 只有累计值。

第三批：team + matchmaking + session。它们暴露了三个此前只在内存里活着的规则，
落库时才发现必须显式化：

- 一人一队（`team_members_one_team_per_player`）：内存版原先允许一个人建两个队，
  现在两端都拒绝。
- 同队列一张 waiting 票（`matchmaking_one_waiting_ticket`）：内存版原先只靠服务层
  先查后写，现在是数据库兜底。
- 一个 match 一场会话（`sessions_one_per_match`）：内存版原先只按 session id 去重。

同时补上了一条审计缺口：`match_results` / `match_player_results` 之前**没有任何代码
写入**，战报只用于发奖、没有留档。现在 `session.submitResult` 在发奖前先落库存档
（按 `result_id` 幂等），失败返回 503 可重试。

## 2. 目标 / 非目标

目标：

1. `BACKEND_DATABASE_URL` 非空时，account 与 economy 走 Postgres；为空时仍走内存，
   `go run ./cmd/platform` 与全部现有测试不需要数据库。
2. 每个迁移的域都有**等价性测试**：同一段脚本分别打到内存实现与 SQL 实现，
   逐步骤比较结果（含拒绝分支）。
3. 装配日志明确写出每个域用的是 `memory` 还是 `postgres`。
4. 启动时连不上库 = 启动失败，而不是运行期 500。

非目标：

- 其余五个域（team、matchmaking、session、progress、rank）的落库。
- 跨域结算的原子性（`sink.go` 目前靠幂等重试收敛，见 ADR 0006 已知债务）。
- `Store` 接口加 `context`、读副本、连接池调优、迁移框架。

## 3. 验收标准

```text
Given BACKEND_DATABASE_URL 指向一个已执行 0001_init.sql 的库
When  两名玩家登录、排队、分配、上报战报、领取奖励
Then  players / wallets / ledger_entries 出现对应行，余额与账本一致

Given 同一份战报重放
Then  账本不新增行，余额不变（唯一键拦截，applied=false）

Given 同一段操作分别打到内存实现与 SQL 实现
Then  每一步的 applied 标志、余额、错误码、账本内容完全一致
```

以上由 `services/account/store_equiv_test.go`、`services/economy/store_equiv_test.go`
与一次真实 `platform + fakeds` 跑通共同覆盖。

## 4. 契约变更

无。对外 21 个 op 的路径、字段、认证级别、幂等声明都没动——这正是"只换 Store
实现"的意义。

## 5. 数据模型变更

无 schema 变更。本批只是把 `db/migrations/0001_init.sql` 里已存在的表用起来：

| 域 | 表 |
|---|---|
| account | `players` |
| economy | `wallets`、`ledger_entries` |
| progress | `player_progress`、`progress_grants`（迁移 0002 新增） |
| rank | `player_ratings`、`rating_events` |
| team | `teams`、`team_members` |
| matchmaking | `matchmaking_tickets` |
| session | `sessions`、`session_players`、`match_results`、`match_player_results` |

## 6. 不变量与失败模式

| 场景 | 数据库行为 | Go 行为 |
|---|---|---|
| 同 `external_id` 二次登录 | 唯一键冲突 | 返回既有玩家，`created=false` |
| 同一幂等键二次入账 | `ledger_entries_idempotent` 冲突 | `applied=false`，返回当前钱包 |
| 扣费超出余额 | `wallets_balance_check` 拒绝 | `412 failed_precondition` + balance/required |
| 给不存在的玩家发钱 | `ledger_entries_player_id_fkey` 拒绝 | `400 invalid_argument` + player_id |
| 空 `external_id` | 两个 store 都拒绝 | `400 invalid_argument`（原先内存侧未校验，已修） |

## 7. 可观测性

- 启动时每个域一行 `store selected`（`domain` + `store`）。
- 账本/钱包的写仍走原有请求日志；数据库错误原样带 SQLSTATE 进日志（不清洗）。
- 无新增指标。连接池指标留到有真实流量再谈。

## 8. 测试计划

| 层 | 用例 |
|---|---|
| 等价性 | account：首次登录 / 重复登录 / 读取 / 改名 / 未知 id / 空 external_id |
| 等价性 | economy：首次入账 / 幂等重放 / 扣费 / 余额不足 / 未知货币 / 金额 0 / 非正数 |
| 等价性 | progress：首次读取 / 入账 / 幂等重放 / 跨级（140+900=1040 → 2 级）/ 空键 / 0 XP |
| 等价性 | rank：首评 1000 / 胜 +24 / 幂等重放不位移 / 负 -16 / 空键 / 62 连败夹到下限 100 / 榜单顺序 |
| 等价性 | team：建队 / 入队 / 重放不重复 / 一人一队被拒 / 未知队 404 / owner 排首位 |
| 等价性 | matchmaking：入队 / 重复票被拒 / 单人不成局 / 两人成局并标记 matched / 空队列 / 未知票 404 |
| 等价性 | session：一场一会话 / 非法跳转被拒 / ready→finished / 落库回读 / 战报留档且重放不重复 / 超时扫成 failed |
| 数据库约束 | `db/probes/0001_constraints.probe.sql`（35 条，psql 直接跑） |
| 端到端 | `platform` + `fakeds`，`BACKEND_DATABASE_URL` 指向真库，检查落库行 |
| 内存路径 | 全部既有测试（`go test ./...`）在无 `BACKEND_DATABASE_URL` 时仍必须全绿 |

DB 相关测试在无 `BACKEND_TEST_DATABASE_URL` 时 **skip**，因此 CI 目前证明不了它们
（CI 无 Postgres 服务，见 `80-ci-cd.md`）。这不是"跑过了"，是"本机跑过了"。

## 9. 回滚方案

- 运行期：去掉 `BACKEND_DATABASE_URL` 即回到内存实现，代码不需要回退。
- 数据：本批只**写**新行，不改既有 schema；回滚镜像不留脏数据（内存实现忽略这些表）。
- 破坏性变更（改表、删列）在 M1 之外，需要单独一次发布 + 前向兼容三步走。

## 10. 未决

- [ ] 其余五个域的落库顺序（建议：progress → rank → session → matchmaking → team）
- [ ] `sink.go` 的多域写入要不要包进一个事务（现在靠幂等重试）
- [ ] `Store` 接口加 `context` 的时机
- [ ] 等价性测试进 CI 的方式（给 workflow 加 postgres service container）