# 部署手册

> 读者：要把它部署到一台机器上的人 · 状态：活文档（2026-11-03） · 上游：ADR 0006 / spec 0006 / 0016 / 0019
>
> 镜像与编排**由 CI 构建、推送并回拉探活**（`image` / `release` job），迁移与备份恢复也在
> `ops` / `compose` job 里跑过。**真正没有验证的是"在一台真机上部署"**：systemd 启停、
> TLS 反代、磁盘水位、跨机恢复。第 10 节列的就是这部分。
>
> 上线之后的观测与运维在 `docs/observability.md`；SLO 目标在 `docs/slo.md`。

## 1. 现状

| 东西 | 有 | 状态 |
|---|---|---|
| `Dockerfile` | 有 | 多阶段 + alpine + 非 root；CI 的 `image` job 构建、以 uid 10001 运行、探活并读 `/metrics` |
| `Dockerfile.ds` | 有 | 参考 DS 镜像；CI 的 `ds` job 在容器里真打一局，并断言缺 `RUNTIME_TOKEN` 时**拒绝启动** |
| `.dockerignore` | 有 | 构建上下文只留 Go 源码，不含 `.git`（因此 build 带 `-buildvcs=false`） |
| `deploy/docker-compose.yml` | 有 | dev：postgres + backend + nakama |
| `deploy/docker-compose.prod.yml` | 有 | 单机生产形态：postgres + backend（+ 可选 `observability` profile 的 prometheus）；`ops` job 校验它缺密钥时**必须失败** |
| `deploy/systemd/gamebackend.service` | 有 | 单机 systemd 单元；`ops` job 跑 `systemd-analyze verify` |
| 镜像仓库 | 有 | `ghcr.io/<owner>/hape:<sha>` 与 `:<sha>-ds`，由 `release` job 推送后**回拉探活** |
| 生产认证器 | 有 | `pkg/httpx/jwtauth.go`：Nakama JWT 校验 + 后端签发玩家 token（ADR 0010） |
| 真实主机部署 | **没有** | 见第 10 节：systemd 启停、TLS 反代、磁盘水位、跨机恢复都未在真机验证 |

## 2. 构建镜像

```bash
docker build -t gamebackend:local .
```

分层：`go.mod` → 模块缓存 → 源码 → 静态二进制 → alpine 运行时。
有 1 个直接依赖（`pgx/v5`，ADR 0006），所以 `go.mod` 层开始有意义：改依赖才失效。

构建参数：

| 参数 | 默认 | 用途 |
|---|---|---|
| `GO_IMAGE` | `golang:1.27-alpine` | 换 builder 镜像，例如内网镜像源 |

```bash
docker build --build-arg GO_IMAGE=registry.internal/golang:1.27-alpine -t gamebackend:local .
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

**这套 compose 已经接好数据库**：`deploy/postgres-init/01-backend.sql` 在首次启动时建
`gamebackend` 库与角色，`backend` 通过 `depends_on: postgres` + `BACKEND_DATABASE_URL`
连上去（七个域全部走 Postgres，ADR 0006）。CI 的 `compose` job 证明它真的能用：灌迁移、
跑 48 条断言，并断言一次登录请求**落进了 `players` 表**。

## 5. 环境变量矩阵

来源是 `pkg/config/config.go`（唯一实现，改这里必须同步改本表）。

| 变量 | 默认 | 说明 |
|---|---|---|
| `BACKEND_ENV` | `dev` | `dev` / `staging` / `prod`，其他值启动即报错 |
| `BACKEND_ADDR` | `:8080`（`cmd/platform` 传入） | 监听地址；容器里用 `0.0.0.0:8080` |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `BACKEND_DEV_AUTH` | `true` | `true` 时挂 `DevAuth`；`false` 时必须注入真实认证器，否则拒绝启动 |
| `BACKEND_SERVICE_CREDENTIALS` | 空 | 服务间凭证集：`name=token[:service+service]`。**推荐**；关闭 dev auth 时至少一条（否则启动失败）。scope 见 11.5.1 |
| `BACKEND_SERVICE_SCOPE_ENFORCE` | `true` | `false` = 观察模式：越权调用照常成功，只记 `would-deny` 日志 |
| `BACKEND_SERVICE_TOKEN` | 空 | 旧版单 token。仍可用（等价于一条 scope 为 `*` 的凭证），但与上面互斥；新部署用凭证集 |
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
| `BACKEND_RATE_LIMIT_PLAYER` / `_BURST` | `100` / `200` | 每玩家每秒令牌数与突发；`0` 关闭这一类 |
| `BACKEND_RATE_LIMIT_SERVICE` / `_BURST` | `1000` / `2000` | 每服务凭证的令牌桶（按凭证名，不按 IP） |
| `BACKEND_RATE_LIMIT_PUBLIC` / `_BURST` | `50` / `100` | 无凭证端点（当前没有）按客户端 IP 计 |

两条硬规则（`Config.Validate` 与 `platform.New` 各拦一条）：

1. `BACKEND_ENV=prod` 且 `BACKEND_DEV_AUTH=true` → 裸启动就失败。
2. `BACKEND_DEV_AUTH=false` → 启动时校验 `BACKEND_JWT_KEY` 与
   `BACKEND_SERVICE_CREDENTIALS`（或旧的 `BACKEND_SERVICE_TOKEN`），缺一个就失败；
   凭证里 scope 指向不存在的 service 也失败（拼错的 scope 永远不会匹配，等于静默失效）。
   **不会**悄悄退回 dev bearer token。JWT 模式已实现（`pkg/httpx/jwtauth.go`）：
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
4. **部署**：`BACKEND_ENV=prod`、`BACKEND_DEV_AUTH=false`、
   `BACKEND_SERVICE_CREDENTIALS=nakama=<secret>:account,ops=<secret2>:*`、
   `BACKEND_JWT_KEY=<Nakama 的 session 加密密钥>`；缺 `BACKEND_JWT_KEY` 会**启动失败**，
   这是正确的失败（宁可起不来，也不要拿 dev token 上生产）。调用方（Nakama 插件、
   定时任务）仍用 `BACKEND_SERVICE_TOKEN` 这个变量名承载自己的那份 secret，值是凭证集里
   对应条目的 token——名字对不上、scope 没覆盖到，都会在第一次调用时 403。
5. **验证**：`/healthz` 200、`/readyz` 200、`/metrics` 里能看到请求计数、日志一行一请求。
6. **观察 5 分钟**：5xx 比例、`backend_requests_total` 是否有异常操作码。

## 8. 回滚

- 代码回滚 = 回退镜像 tag，无数据迁移（v0 无持久化）。
- M1 起有数据库后：**迁移只向前**，回滚镜像前先确认 schema 兼容；破坏性迁移要
  单独一次发布，不跟代码一起。

## 9. 已知坑（第一次构建最可能踩的）

1. **base 镜像 tag 不存在**：`golang:1.27-alpine` 与 `alpine:3.20` 是按 `go.mod`
   的 `go 1.27.3` 挑的，若内网镜像源缺这个 tag，用 `--build-arg GO_IMAGE=` 换掉。
2. **`go mod download` 层报错**：今天 `go.mod` 没有依赖，这层几乎不会失败；一旦
   加了依赖而没提交 `go.sum`，这层会失败——那是真错误，不要去改 Dockerfile。
3. **健康检查一直 unhealthy**：检查的是 `127.0.0.1:8080`，若改了 `BACKEND_ADDR`
   的端口，Dockerfile 里的 `HEALTHCHECK` 要一起改（两处常量，刻意不做 shell 展开）。
4. **Nakama 连不上后端**：默认 `BACKEND_BASE_URL=http://host.docker.internal:8080`
   假设后端在宿主机；后端进容器时要显式设成 `http://backend:8080`。

## 10. 未验证清单与待补

未验证（有文件、**没在真机上跑过**）：

- systemd 启停、TLS 反代、磁盘水位、跨机恢复
- 真实主机上的 compose / systemd 组合（CI 只校验 `config` 与 unit 语法）
- 生产数据库运维：备份策略、复制、连接池调优、密码策略（脚本存在，真机未跑）
- Nakama 在真机上的部署形态（CI 用 compose 跑通登录链路，但不是生产拓扑）

已在 CI 验证过、不再列为待补的：

- 镜像构建 / 非 root / 探活 / `/metrics`（`image`）、推送与回拉（`release`）
- 迁移幂等与 append-only 校验、备份恢复演练（恢复到新库）、pg 客户端版本护栏（`ops`）
- 生产 compose 缺密钥必失败、示例 env 可解析（`ops`）
- 48 条 schema 断言与一次登录真实落库（`verify` / `compose`）
- DS 镜像在容器里完成一局、崩溃重启只结算一次（`ds`）

待补（按优先级）：

1. **在真实主机上部署一次**（roadmap 第 1 项）：systemd + TLS 反代 + 备份恢复演练 + 磁盘水位
2. **厂商账单逐行比对**（observability.md 第 2 节的台账已就绪，缺账单数据）
3. 生产数据库运维（复制/连接池/密码策略）
4. 多副本部署时 reconciler 的单活选主（触发条件：多副本）
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

### 11.5 服务凭证轮换

服务间认证从"一个共享静态 token"变成"一组带名字的凭证"，因此可以**零停机**换密钥。
配置用 `BACKEND_SERVICE_CREDENTIALS`（`name=token`，逗号分隔）：

```bash
# .env（root 0600），第一步：把新凭证加进去，旧的原样保留
BACKEND_SERVICE_CREDENTIALS="nakama=<old>,nakama-v2=<new>"
```

流程（四步，任一步都可停在原地不影响服务）：

1. **加**：把新凭证追加进集合，部署后端。启动日志应列出两个名字：
   `auth selected mode=jwt service_credentials=[nakama nakama-v2]`
2. **验**：用新凭证调一次服务接口（`account.login` 或任意 service op），返回 200；
   用**不在集合里**的字符串调一次，返回 401。这确认的是"新凭证真的被读到"。
3. **迁**：把调用方（Nakama 插件的 `BACKEND_SERVICE_TOKEN`、定时任务、运维脚本）
   逐个换成新凭证。判据是请求日志：`caller=nakama` 归零、`caller=nakama-v2` 出现。
4. **删**：去掉旧条目，再部署一次。旧凭证从此 401。

注意事项：

- **两个变量互斥**：`BACKEND_SERVICE_CREDENTIALS` 与 `BACKEND_SERVICE_TOKEN` 同时出现会
  启动失败。这是刻意的——轮换是"在集合里加一条"，留下旧变量等于让一个操作者以为已退役的
  凭证继续可用。
- **没有宽限期**：删掉即失效。第 3 步没做完就做第 4 步，漏掉的调用方立刻 401（这是好事：
  它比"悄悄用着旧凭证"更早暴露）。
- **名字要可读且稳定**：`caller` 字段是唯一能回答"还有谁在用旧凭证"的证据。
#### 11.5.1 最小权限（scope）与观察期落地

凭证可以声明它**能调哪些服务**（spec 0016）：

```bash
# nakama 只换身份，所以只给 account；运维工具显式给 *；将来的 worker 给 matchmaking+session
BACKEND_SERVICE_CREDENTIALS="nakama=<tok>:account,ops=<tok2>:*,worker=<tok3>:matchmaking+session"
```

- **默认拒绝**：不写 scope = 什么都不能调，不是"什么都能调"。启动日志会写
  `service_credentials=[nakama(account) ops(*)]`，`(none)` 就是漏配。
- **越权是 403 `permission_denied`**（不是 401）：凭证是真的，只是不允许做这件事。
- **拼错的 service 会启动失败**：scope 指向本进程没有的域永远不会匹配，等于静默失效。

落地顺序（**新部署**与**存量部署迁移**不一样，别混）：

```bash
# 新部署：直接强制。示例 env 里的 scope 已被覆盖性测试证明能触达所有 service-auth op，
# 所以第一天强制是安全的，观察期只会让你少一层保护。
BACKEND_SERVICE_SCOPE_ENFORCE=true

# 存量部署改成凭证集时：先观察，确认没有漏配的调用方，再强制。
BACKEND_SERVICE_SCOPE_ENFORCE=false   # 越权照常成功，只累加 would-deny 计数
#   ↓ 部署后观察 24h，判据是一个数字（不是 grep 日志）：
#   backend_service_scope_would_deny_total == 0 且一直为 0
#   ↓ 归零后切回
BACKEND_SERVICE_SCOPE_ENFORCE=true
```

判据用指标而不是日志：

| 指标 | 含义 |
|---|---|
| `backend_service_scope_would_deny_total` | 观察模式下"本会被拒绝"的次数（应恒为 0 才切强制） |
| `backend_service_scope_denied_total` | 强制模式下真正被拒绝的次数（非 0 = 有调用方 scope 不对，日志里有 caller/op） |

两条计数在启动时以 0 注册，所以 `== 0` 是一个可查询的事实，而不是"指标不存在"。
启动日志在观察模式下也会打一行 WARN 提醒"未强制"。

为什么新部署可以直接强制：CI 覆盖的是"仓库里付运的示例凭证能触达所有 op"（一条测试专门
盯这个）；而**你实际部署的凭证集合**（尤其是从旧部署迁移过来的）只有真实流量能验证——
所以迁移走观察期，新部署不用。

### 11.5.2 入站限流（spec 0020）

三类调用方各一个令牌桶，超限返回 **429** + `Retry-After`：

| 类 | 键 | 默认 |
|---|---|---|
| player | 玩家 id | 100/s，burst 200 |
| service | 凭证名 | 1000/s，burst 2000 |
| public | 客户端 IP（当前没有 public op） | 50/s，burst 100 |

- **按身份不按 IP**：反代后面所有调用方共用一个 IP，按 IP 限流会把 NAT 后的玩家互相牵连。
- `X-Forwarded-For` **不信任**：没有可信代理配置之前它是攻击者可控的输入。
- `0` 关闭该类；负数启动即失败。默认值只是"比正常客户端激进得多"的量级，不是容量结论，
  要按 `docs/slo.md` 的真实流量再定（spec 0020 未决项）。
- 计数：`backend_rate_limited_{player,service,public}_total`。
- 多副本时每副本各算各的（M4 之前没有共享计数）。

### 11.6 回滚

1. 镜像回滚：`BACKEND_IMAGE=<上一个 sha>` + `up -d`（无数据迁移时最安全）。
2. 迁移回滚：**不支持向下迁移**。破坏性 schema 变更必须单独发布，先双写/回填再删列。
3. 数据回滚：用备份 + `db-restore.sh`，且先确认备份里的 schema 与目标镜像兼容。

### 11.4.1 归档：把旧对局搬去冷表（spec 0020）

`sessions` 及其子表只增不减。`scripts/db-archive.sh` 把**已结束且已确认释放**、创建时间早于
`ARCHIVE_BEFORE` 的会话搬进 `*_archive`（迁移 0005），源表只留近期：

```bash
# 先看会搬多少（默认 dry-run，不写库）
ARCHIVE_BEFORE=2026-01-01 DATABASE_URL=... sh scripts/db-archive.sh

# 确认后执行
ARCHIVE_CONFIRM=yes ARCHIVE_BEFORE=2026-01-01 DATABASE_URL=... sh scripts/db-archive.sh
```

- `ARCHIVE_BEFORE` **必须显式给**：多久算旧是业务决定，不给默认值。
- 只搬"没人还需要"的会话：`release_pending = false`，且要么厂商已确认停止（`released_at`
  非空），要么从未分配到实例（`allocation_id` 为空）。仍待回收的会话 reconciler 还要用，
  一律不动；`failed` 会话也搬（失败的分配同样是历史，会累积）。
- 单事务，子表先搬；重跑是 no-op。归档表没有外键，所以搬回去也简单
  （`INSERT INTO sessions SELECT ... FROM sessions_archive`）。
- **恢复**（把归档搬回源表，例如客诉查证）：

  ```bash
  RESTORE_SESSION_IDS=s_abc123 DATABASE_URL=... sh scripts/db-archive-restore.sh            # 先看
  ARCHIVE_RESTORE_CONFIRM=yes RESTORE_SESSION_IDS=s_abc123 DATABASE_URL=... \
    sh scripts/db-archive-restore.sh                                                        # 再搬
  ```

  一个事务、按外键顺序；**id 已在源表里就拒绝**——否则归档副本会被删掉而源行保留，等于
  静默丢掉归档里的版本。
- 调度（cron / systemd timer）属部署选择，脚本只保证可重复执行。

### 11.7 这一节里哪些是 CI 验证过的

| 项 | 验证方式 |
|---|---|
| 迁移幂等 + append-only 校验 | `ops` job：跑两次断言 0 applied；篡改副本断言被拒 |
| 备份/恢复可用 | `ops` job：写入 → 备份 → 删除 → **恢复到新库** → 断言行回来（脚本跑在 `postgres:16` 镜像里，锁住客户端版本） |
| 版本不匹配会被拦住 | 本地实跑：桩 `pg_dump` 报 99 拒绝写盘；桩 `pg_restore` 报 99 拒绝执行且不碰库 |
| 恢复需要确认 | `ops` job：不带 `RESTORE_CONFIRM` 断言失败 |
| 生产 compose 缺密钥会失败 | `ops` job：无环境变量时 `config -q` 必须失败 |
| 回收回读 | `pkg/battlealloc` 假厂商：停止后先 `running=true` 再 `false` → 成功；一直 running → 可重试并带状态；404/瞬时 500 → 成功；401 → 永久失败 |
| 回收重试 | `services/session`：回收失败时战报仍 `applied=true` 且会话进 `release_pending`；allocator 恢复后 `RetryReleases` 清标记；持续失败时 attempts 递增 |
| 凭证最小权限 | `pkg/httpx` 解析/语义单测；`platform` 端到端：限 `account` 的凭证调 `session.create` 得 403、观察模式下得 200；两个 scope 计数器分别在两种模式下 +1；覆盖性测试断言示例 env 能触达每个 service-auth op |
| 凭证轮换 | `pkg/httpx` / `platform` 单测：重叠期两条都通过且各自身份正确、移除旧的后旧 401、重复 token 拒绝启动、（dev 与 prod 两条 auth 路径都覆盖） |
| 生产 compose 结构合法 | `ops` job：`--env-file backend.env.example config -q` |
| 归档只搬该搬的 | `ops` job：旧会话与可归档的 `failed` 被搬走，年轻/未确认释放/待回收的留下，重跑 no-op |
| 归档能搬回去 | `ops` job：恢复后回到源表且离开冷表，重复恢复被拒 |
| 告警路由合法 | `ops` job：`amtool check-config`（能证明解析与引用，不能证明送达） |
| systemd 单元语法 | `ops` job：`systemd-analyze verify` |
| 发布镜像可运行 | `release` job：build → 推 ghcr（sha tag）→ 拉回 → 跑起来探活 |

**没有 CI 覆盖的部分**：真正的目标主机（systemd 启停、TLS 反代、磁盘与内存水位）、
镜像仓库的保留策略、跨机器恢复。这些需要一台真机器，见 README 的"未验证"清单。

### 11.8 观测与运维（已独立成篇）

指标、告警规则、分配台账与压测基线都在 **`docs/observability.md`**；SLO 目标在
`docs/slo.md`。部署完成后照着 observability 一文接 Prometheus、看回收状态、跑压测。
