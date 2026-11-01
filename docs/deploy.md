# 部署手册

> **本文件描述的镜像与编排从未被真正构建过**：写它的时候机器上没有 Docker
> （`docker version` 直接失败）。命令按文档意图写，但"能跑"这件事没有证据。
> 第一次真机构建时，先读第 9 节「已知坑」。

## 1. 现状

| 东西 | 有 | 说明 |
|---|---|---|
| `Dockerfile` | 有 | 多阶段构建，alpine 运行时 + 非 root（uid 10001），静态二进制 |
| `.dockerignore` | 有 | 构建上下文只留 Go 源码，不含 `.git`（因此 build 带 `-buildvcs=false`） |
| `deploy/docker-compose.yml` | 有 | postgres + backend + nakama 三个服务 |
| 镜像仓库 | **没有** | 没有 registry，也没有 `docker push` 的目标 |
| CI 构建镜像 | **没有** | CI 只跑 `verify`，不构建镜像（见第 10 节） |
| 生产认证器 | **没有** | 只有 `DevAuth`；`BACKEND_DEV_AUTH=false` 时平台**拒绝启动**，这是有意的 |

## 2. 构建镜像

```bash
docker build -t gamebackend:local .
```

分层：`go.mod` → 模块缓存 → 源码 → 静态二进制 → alpine 运行时。
有 1 个直接依赖（`pgx/v5`，ADR 0006），所以 `go.mod` 层开始有意义：改依赖才失效。

构建参数：

| 参数 | 默认 | 用途 |
|---|---|---|
| `GO_IMAGE` | `golang:1.23-alpine` | 换 builder 镜像，例如内网镜像源 |

```bash
docker build --build-arg GO_IMAGE=registry.internal/golang:1.23-alpine -t gamebackend:local .
```

## 3. 单容器运行

```bash
docker run --rm -p 127.0.0.1:8080:8080 \
  -e BACKEND_ENV=dev \
  -e BACKEND_SERVICE_TOKEN=dev-service \
  gamebackend:local
```

验证：

```bash
curl -s http://127.0.0.1:8080/healthz   # {"status":"ok","service":"platform"}
curl -s http://127.0.0.1:8080/readyz    # {"status":"ready","service":"platform"}
curl -s http://127.0.0.1:8080/metrics | head -5
```

跑一遍对局闭环（容器内数据在内存里，重启即丢）：

```bash
go run ./tools/fakeds        # 在宿主机跑，打 127.0.0.1:8080
```

## 4. compose 整套

```bash
# 只起依赖（后端留在宿主机上用 go run，改代码不用重建镜像）
docker compose -f deploy/docker-compose.yml up -d postgres nakama

# 后端也进容器
docker compose -f deploy/docker-compose.yml up -d --build backend

# 让 Nakama 找容器里的后端，而不是宿主机
BACKEND_BASE_URL=http://backend:8080 \
  docker compose -f deploy/docker-compose.yml up -d
```

端口（全部绑 127.0.0.1，不对外）：

| 端口 | 服务 |
|---|---|
| 8080 | 后端 HTTP |
| 5432 | Postgres |
| 7350 / 7351 / 7349 | Nakama client API / console / gRPC |

**注意**：`backend` 服务今天**没有** `depends_on: postgres`，也**没有**设
`BACKEND_DATABASE_URL`——虽然后端七个域都支持 Postgres，但这套 compose 里还没有
一个给后端用的数据库（现成的 `postgres` 服务只建了 `nakama` 库）。

补法是加一个 `/docker-entrypoint-initdb.d/*.sql` 建 `gamebackend` 库与角色，然后
`backend` 加 `depends_on: postgres` 和 `BACKEND_DATABASE_URL=postgres://...@postgres:5432/gamebackend`。
这一步没有 Docker 无法验证，所以列在待补清单里，而不是写成已经配好。

## 5. 环境变量矩阵

来源是 `pkg/config/config.go`（唯一实现，改这里必须同步改本表）。

| 变量 | 默认 | 说明 |
|---|---|---|
| `BACKEND_ENV` | `dev` | `dev` / `staging` / `prod`，其他值启动即报错 |
| `BACKEND_ADDR` | `:8080`（`cmd/platform` 传入） | 监听地址；容器里用 `0.0.0.0:8080` |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `BACKEND_DEV_AUTH` | `true` | `true` 时挂 `DevAuth`；`false` 时必须注入真实认证器，否则拒绝启动 |
| `BACKEND_SERVICE_TOKEN` | 空 | 服务间 token；关闭 dev auth 时必填（服务调用仍用它，不用 JWT） |
| `BACKEND_JWT_KEY` | 空 | Nakama 的 session 加密密钥（HS256）；`BACKEND_DEV_AUTH=false` 时必填 |
| `BACKEND_JWT_ISSUER` | 空 | 非空时校验 token 的 `iss`（Nakama 默认发 `nakama`） |
| `BACKEND_PLAYER_JWT_KEY` | 空 | 可选：给后端签发的玩家 token 单独一把密钥；空 = 复用 `BACKEND_JWT_KEY` |
| `BACKEND_PLAYER_TOKEN_TTL` | `1h` | 后端玩家 token 的有效期；过期后客户端重放 RPC 换取 |
| `BACKEND_ALLOCATOR` | `local` | `local` / `edgegap`（已实现）/ `gamelift`、`agones`（仍是桩） |
| `BACKEND_EDGEGAP_TOKEN` | 空 | Edgegap API token；`BACKEND_ALLOCATOR=edgegap` 时必填 |
| `BACKEND_EDGEGAP_APP` | 空 | 应用名（当前 `Hape`） |
| `BACKEND_EDGEGAP_VERSION` | 空 | 版本名（当前 `demo-app-lyra`）；空 = 最新版本 |
| `BACKEND_EDGEGAP_LATITUDE` / `_LONGITUDE` | 0 | 没有玩家 IP 时的放置坐标；两者必须同时给 |
| `BACKEND_DATABASE_URL` | 空 | 非空时**全部七个域**走 Postgres；空则全部内存（重启即丢） |
| `BACKEND_REGIONS` | `local` | 逗号分隔的区域列表 |
| `BACKEND_RECONCILE_INTERVAL` | `0`（关闭） | 超时会话清理的周期，如 `30s`；格式错误启动即失败 |
| `BACKEND_SESSION_STALE_AFTER` | `5m` | 会话在 `allocating`/`ready` 停留多久算死；必须为正 |

两条硬规则（`Config.Validate` 与 `platform.New` 各拦一条）：

1. `BACKEND_ENV=prod` 且 `BACKEND_DEV_AUTH=true` → 裸启动就失败。
2. `BACKEND_DEV_AUTH=false` → 启动时校验 `BACKEND_JWT_KEY` 与 `BACKEND_SERVICE_TOKEN`，
   缺一个就失败；**不会**悄悄退回 dev bearer token。JWT 模式已实现（`pkg/httpx/jwtauth.go`）：
   校验 Nakama 的 HS256 签名、`exp`/`iat`、可选 `iss`/`aud`，再把 Nakama 用户 id
   解析成后端 player id（`account.LookupByExternalID`，只读，不会建号）。

## 6. 探针语义

| 端点 | 含义 | 谁该用 |
|---|---|---|
| `/healthz` | 进程活着。**不查任何下游** | 容器 `HEALTHCHECK`、livenessProbe |
| `/readyz` | 能接流量（下游检查挂这里） | readinessProbe / 负载均衡摘挂 |
| `/metrics` | Prometheus 文本格式计数器 | 抓取 |

liveness 一旦依赖数据库，数据库抖一下编排器就会杀健康进程——这是刻意的分工，
不要把下游检查塞进 `/healthz`。

## 7. 首次真正部署的流程

M0 没有可部署目标，所以这一步是模板，不是已验证的流水线：

1. **定镜像版本**：用 git short sha 或语义版本，不用 `latest`（`latest` 无法回滚）。
2. **构建 + 打标**：`docker build -t <registry>/gamebackend:<tag> .`
3. **推送到 registry**（待办：还没有 registry）。
4. **部署**：`BACKEND_ENV=prod`、`BACKEND_DEV_AUTH=false`、`BACKEND_SERVICE_TOKEN=<secret>`、
   `BACKEND_JWT_KEY=<Nakama 的 session 加密密钥>`；缺 `BACKEND_JWT_KEY` 会**启动失败**，
   这是正确的失败（宁可起不来，也不要拿 dev token 上生产）。
5. **验证**：`/healthz` 200、`/readyz` 200、`/metrics` 里能看到请求计数、日志一行一请求。
6. **观察 5 分钟**：5xx 比例、`backend_requests_total` 是否有异常操作码。

## 8. 回滚

- 代码回滚 = 回退镜像 tag，无数据迁移（v0 无持久化）。
- M1 起有数据库后：**迁移只向前**，回滚镜像前先确认 schema 兼容；破坏性迁移要
  单独一次发布，不跟代码一起。

## 9. 已知坑（第一次构建最可能踩的）

1. **base 镜像 tag 不存在**：`golang:1.23-alpine` 与 `alpine:3.20` 是按 `go.mod`
   的 `go 1.23.0` 挑的，若内网镜像源缺这个 tag，用 `--build-arg GO_IMAGE=` 换掉。
2. **`go mod download` 层报错**：今天 `go.mod` 没有依赖，这层几乎不会失败；一旦
   加了依赖而没提交 `go.sum`，这层会失败——那是真错误，不要去改 Dockerfile。
3. **健康检查一直 unhealthy**：检查的是 `127.0.0.1:8080`，若改了 `BACKEND_ADDR`
   的端口，Dockerfile 里的 `HEALTHCHECK` 要一起改（两处常量，刻意不做 shell 展开）。
4. **Nakama 连不上后端**：默认 `BACKEND_BASE_URL=http://host.docker.internal:8080`
   假设后端在宿主机；后端进容器时要显式设成 `http://backend:8080`。

## 10. 未验证清单与待补

未验证（有文件、没跑过）：

- `Dockerfile`、`.dockerignore`、`deploy/docker-compose.yml`（含新增的 `backend` 服务）
- 迁移的**部署**路径：`0001_init.sql` 已在本地 Postgres 16.4 上执行并验证（见
  `docs/postgres-local.md`），但"部署时由谁执行迁移"仍未接（第 7 节第 1 步）
- Nakama 镜像 tag `3.22.0` 与那串启动参数
- `nakama/modules` 插件能否真正编译进镜像（`go build -buildmode=plugin`，本机无网络拉 `nakama-common`）

待补（按优先级）：

1. registry + CI 里的镜像构建与推送任务（`.github/workflows/ci.yml` 目前只镜像 `verify.sh` 的步骤，故意保持一一对应）
2. 生产 `Authenticator`（Nakama JWT 校验），有了它 `prod` 才可能真的启动
3. `db/migrations/0001_init.sql` 的**生产执行路径**（本机已验证可执行；生产需要
   runner 或部署步骤，且要与 SQL store 同批上线）
4. compose 里后端的数据库接线（建库 init 脚本 + `BACKEND_DATABASE_URL`），见第 4 节注意
5. Kubernetes / 单机 systemd 的清单：现在只有 compose，没有生产编排
## 11. 生产目标（单机 compose + systemd）

小团队的上线形态是**一台机器 + compose + systemd**；k8s 清单不在本轮范围。
目标是"跑的是 CI 产出的镜像，而不是宿主上重新构建的东西"。

### 11.1 拓扑

```text
互联网 → 反向代理（TLS） → 127.0.0.1:8080 backend ── compose 内网 ── postgres
                                                                  └─ 数据卷 pgdata
```

- `deploy/docker-compose.prod.yml`：`BACKEND_ENV=prod`（拒绝 dev auth）、
  `read_only` + `tmpfs /tmp`、`no-new-privileges`、日志轮转、postgres **不发布宿主端口**。
- 所有密钥用 `${VAR:?}` 声明：**缺一个就启动失败**，而不是带着空密钥跑起来。
- `deploy/systemd/gamebackend.service` 负责随主机启停 compose；
  密钥放在 `/etc/gamebackend/backend.env`（root 0600，模板见 `backend.env.example`）。

### 11.2 发布

```bash
# CI（release job）已经构建并推送：
#   ghcr.io/<owner>/hape:<git-sha 前 12 位>
# 主机上只做拉取与重启：
set -a; . /etc/gamebackend/backend.env; set +a
export BACKEND_IMAGE=ghcr.io/<owner>/hape:<sha>
docker compose -f deploy/docker-compose.prod.yml pull
docker compose -f deploy/docker-compose.prod.yml up -d
curl -fsS http://127.0.0.1:8080/healthz
```

镜像 tag 用 **git sha**，不用 `latest`：回滚就是把 `BACKEND_IMAGE` 换成上一个 sha 再 `up -d`。

### 11.3 迁移

```bash
DATABASE_URL='postgres://gamebackend:...@127.0.0.1:5432/gamebackend' sh scripts/db-migrate.sh
```

- 每个文件记录 **sha256**；重复执行是 no-op；
- **改一个已应用的迁移会被拒绝**（append-only）——这是脚本里的硬校验，不是约定；
- 迁移先于新镜像上线执行：新代码兼容旧 schema，旧代码不兼容新 schema 的部分要单独一次发布。

### 11.4 备份与恢复演练

```bash
DATABASE_URL=... sh scripts/db-backup.sh /var/backups/gamebackend-$(date +%F).dump
RESTORE_CONFIRM=yes DATABASE_URL=... sh scripts/db-restore.sh /var/backups/gamebackend-2026-01-01.dump
```

- 备份是 `pg_dump --format=custom`，写临时文件后 `mv`，**半截备份不会冒充完整备份**
  （小于 512 字节直接判失败）；
- **`pg_dump`/`pg_restore` 的大版本必须与数据库一致**。客户端比服务端新时，dump 里会带
  老服务端不认的语句（`pg_dump` 17 会写 `SET transaction_timeout = 0`，PG 16 直接报
  `unrecognized configuration parameter`），于是备份当天成功、恢复当天失败。两个脚本都
  自带护栏：备份端客户端版本高于服务端就**拒绝写盘**，恢复端从 dump 文件头读出
  `Dumped by pg_dump version`，比目标服务端新就**拒绝执行**。用 docker 时跑
  `postgres:<服务端大版本>` 镜像即可（CI 就是这么做的）；
- **恢复到新库，不要原地覆盖**。`pg_restore --clean` 按 TOC 顺序 `DROP`，满足不了外键依赖，
  原地恢复在有关系的 schema 上会失败——而"恢复了一半"比"没恢复"更糟。正确姿势：建空库
  → 恢复进去 → 校验 → 把服务指向新库；
- 恢复必须显式 `RESTORE_CONFIRM=yes`：这个命令在凌晨三点跑在错误的库上是真实的故障模式；
- 备份只是备份：**没演练过的恢复不算备份**。CI 的 `ops` job 每次 push 都做一次
  "写入 → 备份 → 删除 → 恢复到新库 → 断言行回来了"。

### 11.5 回滚

1. 镜像回滚：`BACKEND_IMAGE=<上一个 sha>` + `up -d`（无数据迁移时最安全）。
2. 迁移回滚：**不支持向下迁移**。破坏性 schema 变更必须单独发布，先双写/回填再删列。
3. 数据回滚：用备份 + `db-restore.sh`，且先确认备份里的 schema 与目标镜像兼容。

### 11.6 这一节里哪些是 CI 验证过的

| 项 | 验证方式 |
|---|---|
| 迁移幂等 + append-only 校验 | `ops` job：跑两次断言 0 applied；篡改副本断言被拒 |
| 备份/恢复可用 | `ops` job：写入 → 备份 → 删除 → **恢复到新库** → 断言行回来（脚本跑在 `postgres:16` 镜像里，锁住客户端版本） |
| 版本不匹配会被拦住 | 本地实跑：桩 `pg_dump` 报 99 拒绝写盘；桩 `pg_restore` 报 99 拒绝执行且不碰库 |
| 恢复需要确认 | `ops` job：不带 `RESTORE_CONFIRM` 断言失败 |
| 生产 compose 缺密钥会失败 | `ops` job：无环境变量时 `config -q` 必须失败 |
| 生产 compose 结构合法 | `ops` job：`--env-file backend.env.example config -q` |
| systemd 单元语法 | `ops` job：`systemd-analyze verify` |
| 发布镜像可运行 | `release` job：build → 推 ghcr（sha tag）→ 拉回 → 跑起来探活 |

**没有 CI 覆盖的部分**：真正的目标主机（systemd 启停、TLS 反代、磁盘与内存水位）、
镜像仓库的保留策略、跨机器恢复。这些需要一台真机器，见 README 的"未验证"清单。