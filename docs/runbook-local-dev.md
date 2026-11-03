# 本地运行手册

> 读者：第一次跑这个仓库的人 · 状态：活文档 · 最后更新：2026-11-03 · 上游：rules/60

## 1. 最短路径

```bash
cd backend

# 终端 1：起后端（默认 127.0.0.1:8080）
go run ./cmd/platform

# 终端 2：模拟一台 Dedicated Server 跑完一整局
go run ./tools/fakeds
```

期望末尾出现 `OK: full match loop completed`，并打印胜者的 wallet / progress / leaderboard。

```bash
curl -s http://127.0.0.1:8080/healthz   # {"service":"platform","status":"ok"}
curl -s http://127.0.0.1:8080/readyz
curl -s http://127.0.0.1:8080/metrics
```

## 2. 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `BACKEND_ADDR` | `:8080` | 监听地址 |
| `BACKEND_ENV` | `dev` | `dev` / `staging` / `prod`；`prod` 会拒绝 dev auth |
| `BACKEND_DEV_AUTH` | `true` | dev 鉴权：`Bearer dev-player-<id>` / `Bearer dev-service` |
| `BACKEND_SERVICE_TOKEN` | 空 | `BACKEND_DEV_AUTH=false` 时必填 |
| `BACKEND_ALLOCATOR` | `local` | `local` / `gamelift` / `edgegap` / `agones`（后三者未接线，会明确报错） |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `BACKEND_DATABASE_URL` | 空 | v0 未使用（存储是内存） |
| `BACKEND_REGIONS` | `local` | 预留 |

`config.Validate()` 会拒绝：`BACKEND_ENV=prod` + `BACKEND_DEV_AUTH=true`；以及关闭 dev auth 却没给 token。

## 3. 手工打接口（dev 鉴权）

```bash
# 登录（拿 player_id 与 token）
curl -s -X POST localhost:8080/v1/account/sessions \
  -H 'Content-Type: application/json' \
  -d '{"external_id":"nakama-user-1"}'

# 用玩家 token 读自己的档案
curl -s localhost:8080/v1/account/players/p_1 \
  -H 'Authorization: Bearer dev-player-p_1'

# 用服务 token 调内部接口（发经验，必须带 Idempotency-Key）
curl -s -X POST localhost:8080/v1/progress/players/p_1/xp \
  -H 'Authorization: Bearer dev-service' \
  -H 'Idempotency-Key: manual-xp-1' \
  -H 'Content-Type: application/json' \
  -d '{"xp":100,"reason":"manual_test"}'

# 钱包与账本
curl -s localhost:8080/v1/economy/players/p_1/wallet -H 'Authorization: Bearer dev-player-p_1'
curl -s localhost:8080/v1/economy/players/p_1/ledger -H 'Authorization: Bearer dev-player-p_1'
```

## 4. 快速自检清单

| 检查 | 命令 / 期望 |
|---|---|
| 全量门禁 | `scripts\verify.cmd` → `VERIFY OK`（PowerShell 5.1 的机器直接用这个） |
| 契约未漂移 | `go run ./tools/apigen -check` → 无输出、退出码 0 |
| 端到端闭环 | `go test ./e2e -run TestMatchLifecycle -v` |
| 并发安全 | `go test -race ./...`（需 C 编译器 / cgo） |
| 分配失败路径 | `BACKEND_ALLOCATOR=gamelift go run ./cmd/platform` → `session.create` 返回 503 |

## 5. 常见问题

**`go build` 报 `error obtaining VCS status`**
工作树里的 `.git` 对 go 不可读。脚本已带 `GOFLAGS=-buildvcs=false`，手工执行时也加上。

**磁盘/缓存问题**
Go 缓存写进了仓库或系统盘满。verify 脚本把 `GOCACHE`/`GOMODCACHE`/`GOPATH` 放到
`%TEMP%\gamebackend`（Linux: `$TMPDIR/gamebackend`）。手工执行时照抄即可。

**端口被占用**
`BACKEND_ADDR=127.0.0.1:18080 go run ./cmd/platform`。

**改了契约但 `apigen -check` 失败**
正常，说明产物没更新：`go run ./tools/apigen` 后重跑。

**`401 unauthenticated`**
dev 模式下 token 必须是 `dev-player-<player_id>` 或 `dev-service`；
玩家 token 打服务接口会 401（这是设计，不是 bug）。

## 6. 接入真实 Nakama（M3）

1. 起 Postgres + Nakama（见 `deploy/docker-compose.yml`，未验证）。
2. 把 `nakama/modules/` 编译成 `.so` 放进 Nakama 的 `data/modules/`（需要联网拉依赖）。
3. `BACKEND_DEV_AUTH=false` + 真实凭证集：`BACKEND_SERVICE_CREDENTIALS=nakama=<secret>:account`
   （Nakama 插件用同名变量 `BACKEND_SERVICE_TOKEN` 承载这条 secret；scope 必须覆盖它要调的
   service，否则 403）。
4. Nakama 的登录钩子把外部身份转发到 `POST /v1/account/sessions`。

## 7. 接入真实托管战斗服（M2）

1. 在 `pkg/battlealloc` 实现一个适配器（满足 `Allocator` 三个方法）。
2. 在 `battlealloc.New` 里挂上名字。
3. `BACKEND_ALLOCATOR=<vendor>`。
4. 补 `Release` 调用点（对局结束 + 超时两条路径）。
5. 按 `docs/ue5-ds-integration.md` 让 DS 回调 `ready` / `results`。
