# ADR 0006 - Postgres 持久化与驱动选型

- 状态：**proposed（待拍板）**
- 日期：2026-01-01
- 关联：ADR 0004（HTTP/JSON v0）、ADR 0005（模块化单体）、`db/migrations/0001_init.sql`

## 背景

M0 的七个服务全跑在 `services/*/store.go` 的内存实现后面，`Store` 接口就是为换
持久化留的缝。schema 已经写好并在 Postgres 16.4 上验证过约束（`db/probes/`），
但没有一行 Go 代码连过数据库。

接库这一步撞上一条现行政策：**v0 只用标准库，无第三方依赖**（`AGENTS.md`、
`.harness/rules/20-go-style.md`）。标准库里没有 Postgres 驱动，所以 M1 必然要
破这条政策——按 `.harness/rules/10-workflow.md`，这属于"必须停下来问"的决策。

## 选项

| 方案 | 优点 | 代价 |
|---|---|---|
| **A. `github.com/jackc/pgx/v5` + `pgxpool`**（推荐） | 事实上的 Go 生态标准；性能好；`COPY`、`LISTEN/NOTIFY`、类型系统完善；活跃维护（MIT） | 依赖树最大；必须 `go.sum` + 供应链审查；API 与 `database/sql` 不同 |
| B. `github.com/lib/pq` | 小、老牌、纯 `database/sql` 接口 | 已进入维护模式，新特性不再跟进（含部分类型与批量能力） |
| C. `pgx/v5` 的 `stdlib` 适配 + 只用 `database/sql` | 保留 `database/sql` 抽象，将来换驱动容易 | 丢掉 pgx 的批量/类型优势，多一层转接 |
| D. 自己写 wire protocol | 零依赖 | 荒谬：认证、TLS、类型编解码、故障处理全部自己扛 |

## 决定（待确认）

**选 A**：`pgx/v5` + `pgxpool`，驱动只在 `services/*/store.go` 的 SQL 实现里出现，
不泄漏到 handler、契约层或其它包。若某天必须回到 `database/sql`，C 方案是退路。

同时决定：

1. **不引入迁移框架**。迁移仍是 `db/migrations/NNNN_*.sql` 的纯 SQL 文件，
   由部署流程用 `psql`（或将来一个几十行的 runner）执行；`schema_migrations`
   记账只在开发脚本里出现（`scripts/pg-local.ps1`）。
2. **`Store` 接口不变**。SQL 实现新增文件（如 `store_pg.go`），不改接口签名；
   需要事务的跨域写（结算扇出）由 `platform/sink.go` 显式开事务，而不是把
   事务对象塞进 `Store`。
3. **幂等靠唯一约束**，不靠"先查后写"。schema 已经把 `ledger_entries(player_id,
   idempotency_key)`、`rating_events(board, player_id, idempotency_key)`、
   `sessions(result_id)`、`match_results(session_id)` 建成唯一键，重放会撞键，
   由 SQL 实现翻译成"已应用"。

## 后果

- 仓库从"零依赖"变成"有依赖"：`go.mod` 多一条 `require`，CI 需要网络拉模块。
  这是唯一一条值得为 Postgres 破的规则；其它依赖仍然拒绝。
- 需要一次供应链审查：license（MIT）、`go.sum` 提交、更新频率。
- 需要一份"内存实现 vs SQL 实现"的等价性测试：同一组域操作跑两遍，断言两边
  结果一致（M1 的验收标准之一）。
- 若最终选 B，上面除第 1、2、3 条外的结论不变。

## 未决

- 连接池参数（`MaxConns`、超时）什么时候需要调：等有真实并发数据，先按默认。
- 读副本 / 分库：不在 M1 范围。