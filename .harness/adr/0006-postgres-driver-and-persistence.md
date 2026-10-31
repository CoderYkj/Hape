# ADR 0006 - Postgres 持久化与驱动选型

- 状态：**accepted**（2026-01-01 拍板：方案 A，`pgx/v5`）
- 关联：ADR 0004、ADR 0005、`db/migrations/0001_init.sql`、`specs/0002-m1-postgres-stores.spec.md`

## 背景

M0 的七个服务跑在 `services/*/store.go` 的内存实现后面，`Store` 接口就是为换持久化
留的缝。schema 已写好并在 Postgres 16.4 上验证过约束（`db/probes/`），但没有一行
Go 代码连过数据库。

接库撞上一条现行政策：**v0 只用标准库，无第三方依赖**。标准库里没有 Postgres 驱动，
所以 M1 必然要破这条政策——按 `10-workflow.md`，这属于"必须停下来问"的决策。

## 选项

| 方案 | 优点 | 代价 |
|---|---|---|
| **A. `pgx/v5` + `pgxpool`**（采纳） | 生态标准；性能好；批量、类型系统完善；活跃维护（MIT） | 依赖树最大；需要供应链审查 |
| B. `lib/pq` | 小、老牌、纯 `database/sql` | 维护模式，新特性不再跟进 |
| C. `pgx/v5` 的 `stdlib` 适配 | 保留 `database/sql` 抽象 | 丢掉 pgx 的批量/类型优势，多一层转接 |
| D. 自写 wire protocol | 零依赖 | 认证、TLS、类型编解码全自己扛，荒谬 |

## 决定

**A**：`github.com/jackc/pgx/v5` + `pgxpool`，**钉 `v5.7.6`**。

版本不是随手选的：`v5.12.0` 要求 Go ≥ 1.25，会把 `go.mod` 的 `go` 指令从
`1.23.0` 抬到 `1.25.0`，并连带打破 `Dockerfile` 里 `GO_IMAGE=golang:1.23-alpine`
的 builder。`v5.7.6` 只需 Go ≥ 1.21，工具链下限保持不变。升级 pgx 是常规依赖
升级，届时同步升 builder 镜像即可。

驱动只出现在两处：`pkg/pg`（连接池 + 错误翻译）与 `services/*/store_pg.go`。
handler、契约层、`platform` 的服务装配都不 import 驱动类型。

同时决定：

1. **不引入迁移框架**。迁移仍是 `db/migrations/NNNN_*.sql` 的纯 SQL，由部署流程用
   `psql`（或将来一个几十行的 runner）执行；`schema_migrations` 记账只在开发脚本
   （`scripts/pg-local.ps1`）里出现。
2. **`Store` 接口不变**。SQL 实现是新增文件（`store_pg.go`），不改签名。
3. **幂等靠唯一约束**，不靠"先查后写"。`ledger_entries(player_id, idempotency_key)`、
   `rating_events(board, player_id, idempotency_key)`、`sessions(result_id)`、
   `match_results(session_id)` 都是唯一键，重放会撞键，SQL 实现把它翻译成
   "已应用，applied=false"。
4. **迁移按域推进**，不搞一次性大爆炸：每个域一个 `store_pg.go` + 一组等价性测试。
   装配日志明确打印每个域用的是 `memory` 还是 `postgres`。

已迁域：**7/7**——`account`、`team`、`matchmaking`、`session`、`progress`、
`economy`、`rank`。迁移文件两个：`0001_init.sql`（全量 schema）、
`0002_progress_grants.sql`（经验幂等承载表，因为 `player_progress` 只有累计值）。

落库过程中发现三条"只在内存里活着"的规则，现在由数据库兜底、由内存实现对齐：
一人一队、同队列一张 waiting 票、一个 match 一场会话。还补了一个审计缺口：
`match_results` / `match_player_results` 之前没有写入方，战报只用于发钱不留档，
现在 `session.submitResult` 先存档再发奖（`result_id` 幂等）。

两个可选能力接口也在这里出现：`session.Expirer`（超时清理）与
`session.ResultRecorder`（战报留档）。SQL 与内存都实现，接口不变。

## 已知债务（写下来，别假装不存在）

1. ~~**`Store` 方法没有 `context`**~~ → **已还**（spec 0003）：每个 Store 方法第一个
   参数是 `ctx`，SQL 实现用 `context.WithTimeout(ctx, 3s)`，请求取消能一路传到数据库。
   有测试钉住：已取消的 ctx 下查询立刻返回 `context.Canceled`。
2. ~~**跨域结算不是单个事务**~~ → **已还**（ADR 0007 / spec 0005）：数据库模式下
   一次结算（战报 + 三个域 + 会话状态）在同一个事务里提交，失败全回滚。
   内存模式仍只能做到幂等收敛。拆分进程时必须改 outbox，ADR 0007 已写死。
3. ~~**两个无 error 返回的查询会静默降级**~~ → **已还**（spec 0004）：
   `rank.RatingOf` 与 `matchmaking.FindWaitingByPlayer` 都返回 error，调用方把失败
   变成 `503 unavailable`，不再替玩家猜 MMR、也不再把"查不到"当成"没在排队"。
3. **账号与经济必须先一起迁**。`ledger_entries.player_id` 有外键指向 `players`，
   只迁经济不迁账号 → 发钱直接失败。这是刻意的数据完整性，不是耦合失误。

## 踩过的坑（写进测试，不写进脑子）

1. **`ON CONFLICT` 之前就校验 CHECK**。用负数做 `INSERT ... ON CONFLICT DO UPDATE`
   扣钱，会先在"假想的 0-60 行"上撞 `wallets_balance_check`。扣费必须用带守卫的
   `UPDATE ... WHERE balance + $delta >= 0`，只有入账才用 upsert。
2. **语句失败会让整个事务 abort**。撞唯一键后在同事务里再 `SELECT` 只会得到
   `25P02 current transaction is aborted`。重放路径必须先回滚、再在事务外读。
3. **进程内自增的 id 不是 id**。`session` 用 `s_<serial>`、`matchmaking` 用
   `m_<queue>_<serial>`，在内存里够用；接上持久化后，进程重启又从头开始，于是
   `sessions` 主键撞车、`sessions_one_per_match` 把新一轮的对局当成重复。现在
   两者都改成时间基（与 account / team / matchmaking ticket 一致）。教训：
   任何跨进程存在的实体，id 必须由时间或随机数生成，计数器只是方便。
4. **`DROP SCHEMA public; CREATE SCHEMA public` 会清掉授权**。新 schema 不再默认
   给 PUBLIC 权限，应用角色会看到"relation does not exist"。授权属于环境，不是
   schema：`scripts/pg-local.ps1 -Action grant`。

## 后果

- 仓库从零依赖变成有依赖：`go.mod` / `go.sum` 入库，CI 需要能拉模块。
- 拉模块需要镜像：本机直连 `proxy.golang.org` 超时，用
  `GOPROXY=https://goproxy.cn` + `GOSUMDB=off`（`go.sum` 仍会记录校验和，
  只是首次获取不校验 sumdb）。该设置记录在 `60-commands.md`。
- 每个域迁移都需要一步等价性测试（内存 vs SQL 同脚本对拍），这是 M1 的验收标准。

## 未决

- 连接池参数调优：等有真实并发数据，先按默认（`MaxConns=10`）。
- 读副本、分库、分区：不在 M1 范围。
- `Store` 加 `context` 的时机：与"拆进程（M4）"一起做，届时 RPC 边界本来就要传 ctx。