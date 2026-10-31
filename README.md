# gamebackend — 战斗后端脚手架

[![CI](https://github.com/CoderYkj/Hape/actions/workflows/ci.yml/badge.svg)](https://github.com/CoderYkj/Hape/actions/workflows/ci.yml)

UE5 Dedicated Server 扛战斗内权威，Nakama 扛账号/社交，Go 微服务扛
账号/队伍/匹配/会话/进度/经济/排行；早期战斗服走托管厂商，通过抽象
分配层接入。

这个仓库不是"示例代码合集"，而是一份**给 AI Agent 和人共同使用的工程骨架**：
契约、规则、决策、目录、校验脚本都已就位，新功能按规矩往里填，而不是重新发明结构。

## 30 秒跑起来

```bash
go run ./cmd/platform          # 起平台，默认 127.0.0.1:8080
go run ./tools/fakeds          # 另开一个终端：模拟一场完整对局
```

`fakeds` 会走完 `建会话 -> 分配战斗服 -> ready -> 战斗服上报战报 ->
结算发奖 -> 会话 finished`，并打印钱包/经验/评分变化。它同时也是一份
可执行的接口用法文档。

想让它真的落库（七个域都已接 Postgres）：

```powershell
scripts\pg-local.cmd -Action start
scripts\pg-local.cmd -Action migrate
$env:BACKEND_DATABASE_URL = 'postgres://gamebackend:gamebackend@127.0.0.1:55432/gamebackend?sslmode=disable'
go run ./cmd/platform
```

启动日志会打印每个域用的 store（`memory` 或 `postgres`），别靠猜。

全量校验（格式 / vet / build / 测试 / 契约漂移）:

```bash
scripts\verify.cmd                # Windows (PowerShell 5.1 也行)
make verify                       # Linux / CI
```

## 谁该先读什么

| 角色 | 入口 |
|---|---|
| AI Agent | `AGENTS.md` -> `.harness/rules/00-global.md` -> 其余规则按编号 |
| 新人 | 本文件 -> `docs/architecture.md` -> `docs/runbook-local-dev.md` |
| 改接口的人 | `docs/api-contract.md` + `.harness/rules/30-api-contract.md` |
| 接战斗服的人 | `docs/ue5-ds-integration.md` + `.harness/rules/90-battle-integration.md` |
| 要接托管厂商的人 | `docs/edgegap-setup.md`（第一家：控制台要建什么、API 形状、后端配置） |
| 要部署的人 | `docs/deploy.md`（镜像 / compose / 环境变量 / 探针 / 回滚） |
| 要连数据库的人 | `docs/postgres-local.md`（本机免安装实例）+ `.harness/adr/0006`（驱动选型待拍板） |

## 三条不可协商

1. **契约先行**：`api.Op` 是唯一真相源，`api/openapi/*.json` 由
   `go run ./tools/apigen` 生成，`apigen -check` 在 CI 里挡漂移。
   手改生成的 JSON 一律视为错误。
2. **权威边界**：只有战斗服能上报战报；客户端无法提交结果，也无从伪造
   分数。结算只发生在战斗服战报被接受之后。
3. **写操作必须可重试**：每个写接口要么声明 `Idempotent: true`，要么在
   `Note` 里写清为什么不幂等。测试会强制这条。

## 结构

```
cmd/            进程入口（platform = 单体平台；rank = 拆分路径示范）
platform/       装配层：把各服务挂到同一路由 + 结算扇出（sink.go）
services/       7 个领域包，每个都有 DTO + Store 接口 + 内存实现 + 路由注册
                已落库的域另有 store_pg.go（account、economy）与等价性测试
pkg/            基础设施：api(契约) / apierr(错误) / httpx(HTTP) / battlealloc(分配抽象) / pg(驱动)
tools/          apigen(生成 OpenAPI) / fakeds(假战斗服)
api/openapi/    生成的接口契约（勿手改）
db/migrations/  Postgres schema（0001+0002，已在 16.4 上执行；7 个域已接线）
db/probes/      schema 约束的断言脚本（35 条，psql 可重跑）
nakama/         Nakama Go 模块（RPC 桥接，未验证）
deploy/         docker-compose：postgres + backend + nakama（未验证）
Dockerfile      多阶段构建，alpine + 非 root 静态二进制（未验证）
.dockerignore   构建上下文收敛到 Go 源码（刻意不含 .git）
docs/           架构、契约、UE5 对接、本地运行手册、部署手册、Edgegap 接入指引
.harness/       规则 / 规格模板 / ADR
```

## 已验证 vs 未验证

**已验证**（在 Windows + Go 1.27 上实际跑过）：

- `go build ./...`、`go vet ./...`、`go test ./...` 全绿
- `gofmt -l .` 无输出
- `go run ./tools/apigen -check` 无漂移
- `platform` + `fakeds` 完整对局闭环跑通（`OK: full match loop completed`）
- `db/migrations/0001_init.sql` 在真实 Postgres 16.4 上执行成功（13 张表）；
  `db/probes/0001_constraints.probe.sql` **35 条约束断言全 PASS**
  （幂等键、余额非负、状态机守卫、一会话一战报、一人一队都被数据库拒绝）
- **七个域全部落库**：`platform` 连真库跑完 `fakeds` 全链路，14 张表全部出现对应行
  （玩家、排队票、会话、参战名单、战报头 + 每人明细、经验 + 入账流水、钱包 + 流水、
  评分 + MMR 事件）。七个 `services/*` 的「内存 vs SQL」等价性测试逐步骤对齐，
  含幂等重放、余额不足、评分下限、一人一队、一场一会话、非法状态跳转

**未验证**（本机缺 docker / buf / make，且未接真实厂商）：

- `Dockerfile` / `.dockerignore` / `deploy/docker-compose.yml`（含 `backend` 服务）：
  本机没有 Docker，`docker build` 与 `compose up` 都没跑过；详见 `docs/deploy.md`
- `nakama/modules`：独立 Go module，`nakama-common` 版本号为占位，
  没编译过，没跑过
- **没有生产数据库**：schema 只在本地实例与 CI 的临时容器上跑过，没有备份、
  复制、连接池调优与密码策略（`docs/postgres-local.md` 的"未验证/未做"一节）
- `docs/ue5-ds-integration.md` 里的 C++ 片段：未在 UE5 工程里编译
- 真实托管战斗服（GameLift / EdgeGap / Agones）：只有接口与桩实现
- **CI 全绿**（GitHub Actions，`ubuntu-latest`）：`gofmt` / `go vet` / `go build` /
  `go test -race`（含真 Postgres 里的七个等价性测试）/ schema 探针 / 契约漂移。
  本机没有 C 编译器，`-race` 只能在这里真跑——所以它不是"本机已验证"，
  而是"CI 已验证"。

不要因为某文件"看起来完整"就假设它跑通了——以这一节为准。

## 当前实现边界

- 存储有两种模式：**不设 `BACKEND_DATABASE_URL` 全部走内存**（`go run` 与测试零依赖），
  **设了则七个域全部走 Postgres**，进程重启数据还在。启动日志逐域打印
  `store selected`，不靠猜。
- 内存实现仍然保留且必须与 SQL 实现行为一致——`services/*/store_equiv_test.go`
  就是这条约束的可执行版本。
- **结算原子性只在数据库模式成立**：一次扇出（战报 + 进度 + 经济 + 排行 + 会话状态）
  在同一个事务里提交。内存模式下各 store 持有各自的 map，做不到跨域事务，
  只能靠幂等重试收敛（ADR 0007）。
- 所有 `Store` 方法都带 `context.Context`（spec 0003）：请求取消会一路传到数据库，
  不会出现"客户端已断开、查询还在跑"。
- 查询失败一律显式报错（spec 0004）：`rank.RatingOf` 与
  `matchmaking.FindWaitingByPlayer` 不再静默降级为"默认 1000 分 / 没在排队"，
  失败返回 503 可重试。
- 分配器只有 `local` 实现（返回 `127.0.0.1:7777+n`）；厂商实现是返回
  `ErrNotConfigured` 的桩。
- 单体单进程：7 个服务在同一个二进制里，按包边界隔离；`cmd/rank` 证明
  拆出去只需一个 main。
- 认证有两套：开发态（`Bearer dev-player-<id>` / `Bearer dev-service`）与生产态
  （Nakama 的 HS256 JWT）。`BACKEND_DEV_AUTH=false` 时切换，缺 `BACKEND_JWT_KEY`
  启动即失败。
- 会话回收与超时对账已接：结算后 `Release` 一次、`ExpireStale` 清理停在
  `allocating`/`ready` 的会话并 `Release`。定时器默认**关**，用
  `BACKEND_RECONCILE_INTERVAL` 打开（多实例还没有单活选主）。
- 战斗服鉴权用共享 service token，没有 per-allocation token（见 ADR-0003）。

## 已知缺口（生产前必须解决）

| 缺口 | 为什么危险 | 关闭方式 |
|---|---|---|
| `account.login` 仍是公共 op，信任 body 里的 `external_id` | 任何人可以声称自己是任意 Nakama 用户，凭空拿到那个玩家的后端 id 与 token | 新增"用 JWT 换后端身份"的 op（`AuthPlayer`，身份取自 token 而非 body），把现在这个降级为仅 dev；属于改契约，需要先拍板 |
| 服务间认证是共享静态 token | 一个泄露的 token 就能伪造战报 | per-allocation token（M2，`sessions.runtime_token` 字段已建） |
| 没有生产数据库运维 | 无备份、复制、连接池调优、密码策略 | 运维上车时补，见 `docs/postgres-local.md` 的"未验证/未做" |
| `nakama/modules` 从未编译通过 | 登录链路在生产里跑不起来 | CI 里加了编译步骤（见第 3 项），失败就修 |

## 路线图

| 阶段 | 目标 |
|---|---|
| M0 ✅ | 垂直切片：单机跑通完整对局闭环（当前状态） |
| M1 ✅ | 接 Postgres：7/7 域落库，14 张表全部有写入，等价性测试对齐（spec 0002） |
| M2 | 接真实托管分配器：填 `pkg/battlealloc` 的厂商实现（回收与超时对账已接，缺的是厂商侧确认） |
| M3 | Nakama 落地：账号/RPC 桥接跑通，客户端登录链路打通 |
| M4 | 服务拆分：rank / matchmaking 独立进程，加 gRPC 或消息队列 |
| M5 | 可观测性与容量：指标落地、压测、SLO |

## 许可

内部脚手架，未附带 license 文件；对外发布前需补。