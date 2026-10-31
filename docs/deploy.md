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

**注意**：`backend` 服务今天**不** `depends_on: postgres`——v0 的存储全在内存，
它跟数据库还没关系。M1 接上 SQL store 后才加这个依赖和 `BACKEND_DATABASE_URL`。
现在写上去只会让人误以为"存档已持久化"。

## 5. 环境变量矩阵

来源是 `pkg/config/config.go`（唯一实现，改这里必须同步改本表）。

| 变量 | 默认 | 说明 |
|---|---|---|
| `BACKEND_ENV` | `dev` | `dev` / `staging` / `prod`，其他值启动即报错 |
| `BACKEND_ADDR` | `:8080`（`cmd/platform` 传入） | 监听地址；容器里用 `0.0.0.0:8080` |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `BACKEND_DEV_AUTH` | `true` | `true` 时挂 `DevAuth`；`false` 时必须注入真实认证器，否则拒绝启动 |
| `BACKEND_SERVICE_TOKEN` | 空 | 服务间 token；`prod` 或关闭 dev auth 时必填 |
| `BACKEND_ALLOCATOR` | `local` | `local` / `gamelift` / `edgegap` / `agones`；后三者是返回 `ErrNotConfigured` 的桩 |
| `BACKEND_DATABASE_URL` | 空 | 非空时**全部七个域**走 Postgres；空则全部内存（重启即丢） |
| `BACKEND_REGIONS` | `local` | 逗号分隔的区域列表 |
| `BACKEND_RECONCILE_INTERVAL` | `0`（关闭） | 超时会话清理的周期，如 `30s`；格式错误启动即失败 |
| `BACKEND_SESSION_STALE_AFTER` | `5m` | 会话在 `allocating`/`ready` 停留多久算死；必须为正 |

两条硬规则（`Config.Validate` 与 `platform.New` 各拦一条）：

1. `BACKEND_ENV=prod` 且 `BACKEND_DEV_AUTH=true` → 裸启动就失败。
2. `BACKEND_DEV_AUTH=false` 且没有注入 `Authenticator` → 启动失败，**不会**悄悄
   退回 dev bearer token。生产认证器属于 M3（Nakama JWT 校验）。

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
4. **部署**：`BACKEND_ENV=prod`、`BACKEND_DEV_AUTH=false`、`BACKEND_SERVICE_TOKEN=<secret>`；
   注意第 5 节第 2 条——M3 之前这样配会**启动失败**，这是正确的失败。
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
4. Kubernetes / 单机 systemd 的清单：现在只有 compose，没有生产编排