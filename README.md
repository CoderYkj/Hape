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

想验证"凭证真的进了战斗服实例"（而不是本进程自己扮演 DS）：

```bash
go run ./tools/fakeds -launch-ds exec     # 分配后启动 ds/ 二进制，环境变量注入
docker build -f Dockerfile.ds -t hape-ds:ci . && \
  go run ./tools/fakeds -launch-ds docker -ds-image hape-ds:ci
```

`ds/` 是参考 Dedicated Server（Go，不是 UE5 构建），环境契约见 spec 0012。

再往前一步：**客户端真的进一局**（三个独立进程：客户端 / 厂商 / 战斗服）：

```bash
IDENTITY=dev CLIENT_FULL=1 QUEUE=ue5-drill sh scripts/ds-match-drill.sh
```

客户端打印 `ECHOES=5`（发 5 个包收回 5 个）、`SESSION_STATE=finished`、`TOTAL_XP=125`。
`IDENTITY=nakama` 走真实 Nakama 那条链路（spec 0013）。

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
| **先看整体** | `docs/architecture-map.html`（浏览器打开）：现在长什么样，每个环节做什么，**逐项标注 ✅已验证 / 🟡已实现未验证 / ⬜未实现**；Markdown 源 `docs/architecture-map.md` |
| **看还剩什么** | `docs/roadmap.html`：三条并行主线、现在就能做的三项（含判据）、里程碑 M2–M5、未实现清单、已关闭清单；Markdown 源 `docs/roadmap.md` |
| AI Agent | `AGENTS.md` -> `.harness/rules/00-global.md` -> 其余规则按编号 |
| 新人 | 本文件 -> `docs/architecture-map.md`（图）-> `docs/architecture.md`（细节）-> `docs/runbook-local-dev.md` |
| 改接口的人 | `docs/api-contract.md` + `.harness/rules/30-api-contract.md` |
| 接战斗服的人 | `docs/ue5-ds-integration.md` + `.harness/rules/90-battle-integration.md` |
| 写客户端的人 | `docs/ue5-client-model.md`（客户端只做五件事 + UE5 转写指引）+ `clients/minimal`（可跑的最小实现，`CLIENT_FULL=1` 会连真实战斗服） |
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
tools/          apigen(生成 OpenAPI) / fakeds(假战斗服 + 厂商侧启动演练)
ds/             参考 Dedicated Server：环境契约 + 幂等结算的可执行版本（spec 0012）
api/openapi/    生成的接口契约（勿手改）
db/migrations/  Postgres schema（0001+0002，已在 16.4 上执行；7 个域已接线）
db/probes/      schema 约束的断言脚本（41 条，psql 可重跑）
nakama/         Nakama Go 模块（RPC 桥接，未验证）
deploy/         dev compose + 生产 compose（prod 用占位密钥拒绝启动）+ systemd 单元 + prometheus 告警规则
Dockerfile      后端镜像：多阶段构建，alpine + 非 root 静态二进制（CI 构建并探活）
Dockerfile.ds   战斗服镜像：同一套构建约束，udp 7777（CI 构建并在容器里打一局）
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
  `db/probes/*.probe.sql` **41 条约束断言全 PASS**（0001 的 35 条 + 0003 的 6 条）
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
- **客户端对局闭环**（spec 0013）：`clients/minimal` 从"入队"一路走到"连上战斗服"——
  按票据上的 `match_id` 用 `GET /v1/session/sessions/by-match/{match_id}` 找到地址，
  打开 UDP、发 5 个包收回 5 个，再等 `finished` 用**自己的** token 读收益。
  CI 两个 job 各跑一遍：`ds` job 用 `IDENTITY=dev`（不依赖 Nakama），
  `nakama` job 用 `IDENTITY=nakama`（真设备登录 + RPC 兑换）。本机实跑数字：
  `ECHOES=5`、`SESSION_STATE=finished`、`TOTAL_XP=125`。
  仍未验证的是**UE5 转写**：`docs/ue5-client-model.md` §6 的 C++ 片段没在引擎里编译过。
- 真实托管战斗服：Edgegap 适配器 **已真机跑通**（分配 3s 就绪、拿到
  `172.237.14.252:32719`、释放后状态 TERMINATED）
- **自有 DS 镜像 + 每场凭证容器内验证**（spec 0012）：`ds/` 参考战斗服在容器里跑完
  `ready → 打一局 → 战报`，启动第一行打印 `runtime_token=present(len=N)`（只打长度）；
  CI 的 `ds` job 断言容器内真打一局、进程形态再打一局、崩溃重启后**只结算一次**，
  以及缺 `RUNTIME_TOKEN` 时容器**拒绝启动**。本机 `-launch-ds exec` 实跑通过，
  `-crash-restart` 两次启动复用同一个 `result_id`。
  **仍未复验**：Edgegap 的 `env_vars` 是否真的落进实例环境（机制已验证，厂商侧待真机）
- **CI 全绿**（GitHub Actions，`ubuntu-latest`，8 个 job）：`verify`（`gofmt` / `go vet` /
  `go build` / `go test -race`，含真 Postgres 里的七个等价性测试 / schema 探针 / 契约漂移）、
  `image`、`compose`、`nakama-module`、`nakama`、`ops`、`ds`、`release`。
  本机没有 C 编译器，`-race` 与插件编译只能在这里真跑——所以它们不是"本机已验证"，
  而是"CI 已验证"。
- **镜像与编排**：CI 里 `docker build` 后以非 root（uid 10001）运行并探活；
  `docker compose up postgres backend` 灌迁移、跑 41 条探针，并断言一次登录请求
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
| ~~服务间认证是共享静态 token，换不了~~ | — | **已修复轮换**（spec 0014）：`BACKEND_SERVICE_CREDENTIALS` 支持多条带名字的凭证并行，四步零停机轮换；认证结果带调用方名字并进请求日志（`caller=`），"还有谁在用旧凭证"因此可回答 |
| ~~一条凭证仍能调用所有 service 接口~~ | — | **已修复**（spec 0016）：凭证可声明可调 service（`name=token:account`），越权返回 403；默认拒绝、拼错 scope 启动失败、观察模式（`BACKEND_SERVICE_SCOPE_ENFORCE=false`）先验证再强制；一条覆盖性测试防止示例 env 漏配。余下是 mTLS/短期令牌（M4） |
| 没有生产数据库运维 | 无备份、复制、连接池调优、密码策略 | 运维上车时补，见 `docs/postgres-local.md` 的"未验证/未做" |
| `nakama/modules` 从未编译通过 | 登录链路在生产里跑不起来 | CI 里加了编译步骤（见第 3 项），失败就修 |

**`account.login` 的信任问题：已决定延后**（2026-01-01）。理由：它只在"后端直接暴露给
不可信客户端"时才可被利用；当前部署形态是 Nakama 插件用服务凭证调它。重开条件（满足任一
就立刻做）：① 后端端口对公网或半可信网络开放；② 客户端直连后端换 token；③ 上线前安全
评审。修复方向已写在 `spec 0006` §6：新增一个 JWT 换身份的 op，把现有这个降级为 dev-only。

### 已延后清单（有触发条件，不是"以后再说"）

| 项 | 为什么现在不做 | 什么信号触发它 | 到时的做法 |
|---|---|---|---|
| Edgegap 侧 `env_vars` 复验 | 容器内注入已由 `ds/` + CI `ds` job 验证（spec 0012），但那是本地/CI 的容器；Edgegap 是否把 `env_vars` 真的放进实例环境，需要一个指向自有镜像的 app version 才能看 | 首次真实联调 / 第一次用自有镜像在 Edgegap 分配 | 把 `:<sha>-ds` 镜像在 Edgegap 建 app version，分配一次，看 container logs 里那行 banner |
| 计费对账（不是回收） | 回收**已实测**（spec 0015，2026-11-01）：分配 4s 就绪、释放 `running=false` 用时 2.0s/0.7s、序列 `Status.READY → Status.TERMINATING`、之后 deployments 归零。仍未验证的是 `TERMINATING` 窗口内是否还有秒级计费 | 首次真实计费对账 / M2 收尾 | 用厂商账单 API 或控制台对账一次「释放时刻 vs 计费停止时刻」 |
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