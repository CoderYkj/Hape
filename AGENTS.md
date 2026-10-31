# AGENTS.md

本文件是 AI Agent（DeepSeek Harness / CLI Agent）在本**游戏服务端仓库**工作的总入口。
任务开始前必须先读本文件，再按顺序读 `.harness/rules/00 ~ 95`。

---

## 1. 这是什么

一套面向小团队的游戏后端骨架，三个平面分工明确：

| 平面 | 技术 | 负责 | 不负责 |
|---|---|---|---|
| 社交/账号平面 | **Nakama**（Go 插件） | 登录鉴权、好友、公会、聊天、直连匹配、云存档入口 | 任何需要审计的经济与进度逻辑 |
| 领域平面 | **Go 微服务**（本仓库主要代码） | 账号映射、队伍、匹配、会话、进度、经济、排行 | 帧同步、物理、战斗判定 |
| 战斗平面 | **UE5 Dedicated Server**（早期托管：GameLift / Edgegap / Agones） | 局内唯一权威：命中、伤害、胜负、掉落 | 局外资产、跨局状态、付款 |

一句话：**战斗中 UE5 DS 说了算，战斗之外 Go 说了算，Nakama 只做它擅长的社交与鉴权。**

- 语言：Go（`go 1.23`，v0 只用标准库，无第三方依赖）
- 传输：HTTP/JSON（v0），契约由 Go 声明自动生成 OpenAPI，gRPC 见 ADR 0004
- 存储：v0 内存实现挂在 `Store` 接口后；Postgres 迁移见 `db/migrations`
- 部署形态：v0 单进程模块化单体（`cmd/platform`），拆分路径见 ADR 0005
- 主分支：`main`，Agent 分支前缀 `vibe/`

---

## 2. 快速命令

在本仓库根目录执行（Windows 用 `scripts\verify.ps1`，Linux/CI 用 `make verify`）：

| 目的 | 命令 |
|---|---|
| 本地起全套 | `go run ./cmd/platform` → http://localhost:8080/healthz |
| 模拟一场完整对局 | `go run ./tools/fakeds` |
| 单测 + e2e | `go test ./...` |
| 竞态检查 | `go test -race ./...` |
| 静态检查 | `go vet ./...` |
| 格式 | `gofmt -l .`（有输出即失败）/ `gofmt -w .` |
| 重生成契约 | `go run ./tools/apigen` |
| 契约漂移检查 | `go run ./tools/apigen -check` |
| 全量校验 | `pwsh -File scripts/verify.ps1` 或 `make verify` |

`verify` 依次执行：`gofmt -l` → `go vet` → `go build` → `go test` → `apigen -check`，任一失败即整体失败。

`-race` 需要 cgo（即需要 C 编译器）。本机没有时 `scripts\verify.ps1 -Race` 打印 SKIP 并继续；CI 的 Linux runner 上有 gcc，那里是真跑的。别把 SKIP 当通过。

---

## 3. 上下文加载顺序

1. `AGENTS.md`（本文件）
2. `.harness/rules/00-global.md`
3. `.harness/rules/10-workflow.md`（spec-first 流程与 Definition of Done）
4. `.harness/rules/20-go-style.md`
5. `.harness/rules/30-api-contract.md`
6. `.harness/rules/40-testing.md`
7. `.harness/rules/50-git.md`
8. `.harness/rules/60-commands.md`
9. `.harness/rules/70-security-forbidden.md`
10. `.harness/rules/80-ci-cd.md`
11. `.harness/rules/90-battle-integration.md`（改战斗/匹配/会话必读）
12. `.harness/rules/95-observability.md`
13. 相关 `.harness/specs/*.md`、`.harness/adr/*.md`、源码与测试

---

## 4. 三条不可协商的红线

1. **战斗权威不可上移**：局内结果只能由 Dedicated Server 上报；客户端上报的任何分数、余额、胜负一律不可信（见 `90-battle-integration.md`）。
2. **钱必须可审计**：货币只走整数 + 账本 + 幂等键；任何改动余额的路径都要能凭 `result_id` / `Idempotency-Key` 重放而不重复结算（见 `70-security-forbidden.md`）。
3. **契约不可手改**：OpenAPI 是 `go run ./tools/apigen` 的产物，改契约必须改 Go 声明；手改 `api/openapi/*.json` 会被 `apigen -check` 判失败（见 `30-api-contract.md`）。

---

## 5. 仓库地图

```text
cmd/platform/          单进程入口（组装全部服务）
cmd/rank/              单服务拆分示例（证明拆分路径）
platform/              装配层：路由、依赖注入、比赛结果扇出（sink.go）
services/<domain>/     account team matchmaking session progress economy rank
  store.go             类型 + Store 接口 + 内存实现（SQL 落点）
  service.go           契约声明（api.Op）+ handler + 领域方法
pkg/api/               契约层：Op/Shape/OpenAPI 生成与校验
pkg/apierr/            唯一错误模型（code ↔ HTTP 状态）
pkg/httpx/             路由、鉴权、限流位、指标、健康检查
pkg/battlealloc/       战斗服分配抽象层（local + 三个 vendor 占位）
pkg/config, pkg/logx/  环境变量配置、结构化日志
tools/apigen/          契约 → OpenAPI 生成器（含 -check）
tools/fakeds/          假 Dedicated Server，跑通整条对局链
e2e/                   端到端闭环测试（HTTP 真实请求）
api/openapi/*.json     生成产物，勿手改
nakama/modules/        Nakama Go 插件（独立 go module，需联网构建）
db/migrations/         目标 SQL schema（v0 未启用）
deploy/                本地 docker-compose：postgres + backend + nakama（未验证）
Dockerfile             多阶段构建 + 非 root 运行时（未验证，见 docs/deploy.md）
docs/                  架构 / 接口契约 / UE5 对接 / 本地运行手册
scripts/               verify.ps1 + verify.sh（唯一的全量校验入口）
Makefile               Linux / CI 的等价入口
README.md              人类入口：这是什么、怎么跑、哪些还没验证
.harness/rules|specs|adr/  本脚手架的规范层
```

---

## 6. 沟通要求

- 中文回复，代码/命令/提交信息用英文。
- 先结论，后细节；不确定就说不确定，并给出验证方法。
- 不编造包名、路由、字段、环境变量——写之前先读代码。
- 不声称"已测试 / 已通过"而没有实际执行命令并贴出输出。
- 汇报必须包含：改了什么文件、跑了什么命令、输出结论、未验证的部分。
