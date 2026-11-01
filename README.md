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
| 写客户端的人 | `docs/ue5-client-model.md`（客户端只做四件事 + UE5 转写指引）+ `clients/minimal`（可跑的最小实现） |
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
deploy/         dev compose + 生产 compose（prod 用占位密钥拒绝启动）+ systemd 单元
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

- **从未部署到任何服务器**：生产形态（单机 compose + systemd）、发布（ghcr + sha tag）、
  迁移脚本、备份/恢复脚本都已就位并由 CI 验证（`ops` / `release` job），但
  **没有真机器跑过**：systemd 启停、TLS 反代、磁盘水位、跨机器恢复都未验证
- Nakama 插件只验到"能编译成 `.so`"：**没有真的加载进 Nakama 跑过一次登录**
- **没有生产数据库**：schema 只在本地实例与 CI 的临时容器上跑过，没有备份、
  复制、连接池调优与密码策略（`docs/postgres-local.md` 的"未验证/未做"一节）
- `docs/ue5-ds-integration.md` 里的 C++ 片段：未在 UE5 工程里编译
- Nakama 桥接（Lua 模块）：**CI 在真 Nakama 上验证过**——设备登录 → RPC `login` →
  后端 `account.login` → 后端读取，并在 `players` 表里断言到对应行（spec 0009）。
  仍未验证的是**客户端侧**：UE5/浏览器如何获取 Nakama session、调用 RPC、再用返回的
  后端 token 调后端。
- 真实托管战斗服：Edgegap 适配器 **已真机跑通**（分配 3s 就绪、拿到
  `172.237.14.252:32719`、释放后状态 TERMINATED）；**未确认**的是
  `RUNTIME_TOKEN` 是否真的出现在容器内环境（需要换成我们自己的 DS 镜像打日志）
- **CI 全绿**（GitHub Actions，`ubuntu-latest`，4 个 job）：`gofmt` / `go vet` /
  `go build` / `go test -race`（含真 Postgres 里的七个等价性测试）/ schema 探针 /
  契约漂移。本机没有 C 编译器，`-race` 只能在这里真跑——所以它不是"本机已验证"，
  而是"CI 已验证"。
- **镜像与编排**：CI 里 `docker build` 后以非 root（uid 10001）运行并探活；
  `docker compose up postgres backend` 灌迁移、跑 35 条探针，并断言一次登录请求
  真的落进了 `players` 表。
- **Nakama 插件**：CI 里 `go build -buildmode=plugin` 编译通过（`nakama-common`
  v1.35.0 已解析并提交 `go.sum`）。

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
| ~~`account.login` 信任 body 的 `external_id`~~ | — | **已修复**（ADR 0010）：登录改为服务专属，客户端不再能调用；生产 token 由后端签发（`iss=gamebackend`） |
| 服务间认证仍是共享静态 token | 一个泄露的 token 能调用其它 service 接口（但不能伪造别的场次的战报——上报已要求每场 `X-Session-Token`，spec 0008） | 凭证轮换或 mTLS（M4） |
| 没有生产数据库运维 | 无备份、复制、连接池调优、密码策略 | 运维上车时补，见 `docs/postgres-local.md` 的"未验证/未做" |
| `nakama/modules` 从未编译通过 | 登录链路在生产里跑不起来 | CI 里加了编译步骤（见第 3 项），失败就修 |

**`account.login` 的信任问题：已决定延后**（2026-01-01）。理由：它只在"后端直接暴露给
不可信客户端"时才可被利用；当前部署形态是 Nakama 插件用服务凭证调它。重开条件（满足任一
就立刻做）：① 后端端口对公网或半可信网络开放；② 客户端直连后端换 token；③ 上线前安全
评审。修复方向已写在 `spec 0006` §6：新增一个 JWT 换身份的 op，把现有这个降级为 dev-only。

### 已延后清单（有触发条件，不是"以后再说"）

| 项 | 为什么现在不做 | 什么信号触发它 | 到时的做法 |
|---|---|---|---|
| 自有 DS 镜像 + `RUNTIME_TOKEN` 容器内验证 | 样例镜像是原版 UE5 Lyra，不会打印环境变量；没有自有镜像就没有验证材料 | 第一版自有 DS 镜像就绪 / 首次真实对局联调 | 启动时打一行「已加载 MATCH_ID / SESSION_ID / RUNTIME_TOKEN（只打长度）」，再分配一次看 container logs |
| 厂商侧回收回读确认 | 现在 `DELETE` 后靠人工 `GET /status` 确认 `TERMINATED`；对局结束即释放，尚未计费对账 | 首次真实计费对账 / M2 收尾 | `Release` 里加一次状态回读，`running==false` 才算成功，否则按可重试处理 |
| `account.login` 换身份 op | 见上一节的理由 | 后端对不可信客户端开放 / 客户端直连换 token / 上线前安全评审 | 新增 `AuthPlayer` 的换身份 op，旧 op 标 dev-only（spec 0006 §6） |
| GameLift / Agones 适配器 | 一家够用；三家的适配器结构差异大 | 选定第二家厂商 / 多地合规要求 | 照 `pkg/battlealloc/edgegap.go` 的结构写，契约测试用假服务 |

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