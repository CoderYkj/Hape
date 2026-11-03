# 80 - CI 与交付

## CI 必须通过的作业（顺序即依赖）

```yaml
1. format   gofmt -l .                  # 有输出即失败
2. vet      go vet ./...
3. build    go build ./...
4. test     go test -race ./...
5. contract go run ./tools/apigen -check
```

失败优先级：format → vet → build → test → contract。前一个失败不跑后面（省 CI 时间，错误更聚焦）。

CI 除了 `verify` 还有三个 job，专做本机做不了的事：

| job | 内容 |
|---|---|
| `image` | `docker build` → 非 root 运行 → `/healthz`·`/readyz`·`/metrics` → 断言 uid=10001 |
| `compose` | `compose up postgres backend` → 迁移 → 48 条探针 → 断言一次登录落库 |
| `nakama-module` | 插件的快速编译检查（`go vet` + `-buildmode=plugin`，**不是部署产物**）；**真构建**在 `nakama` job：`docker compose build nakama` 用官方 pluginbuilder 编 `.so` |
| `nakama` | **现役桥接的唯一验证**：构建插件镜像 → 起 compose 全套（postgres + backend + nakama）→ 迁移 → 断言 **Go 插件**被加载（`gamemodule loaded`）→ 用 `tools/nakama-smoke` 走完「Nakama 设备登录 → `login` RPC → 后端账号 → 后端读取」→ 断言 players 表有对应行 |

`nakama` job 的关键是**区分两种失败**：Nakama 起来了但桥接模块没加载，与模块加载了但桥接
逻辑错。前者靠 grep 日志里的 `gamemodule loaded`，后者靠 smoke 探针的逐步输出。

> 历史背景：Go 插件最初在 CI runner 上用任意工具链编，`plugin.Open` 报包哈希不匹配
> （宿主镜像带 `--trimpath` 构建），一度改用 Lua（ADR 0009）。**ADR 0011 已撤回该决定**：
> 插件改在官方 `nakama-pluginbuilder` 镜像里、同 tag、`--trimpath` 构建，Lua 模块已删除。
> 换 Nakama 版本时，`nakama/Dockerfile` 的 tag、`nakama-common` 版本、运行时镜像三者必须一起动。
> 这句话现在有门禁：`scripts/nakama-version-check.sh` 从该 Nakama 版本自己发布的 go.mod 里读
> `nakama-common` 的应有版本，比对 Dockerfile ARG / compose 默认 tag / `go.mod` 三处；
> `nakama-module` 校验源码，`nakama` job 用镜像里的 `gamemodule.buildinfo` 校验实际加载的 `.so`。

`-race` 在 CI 必开：这个后端到处是 `sync.Mutex` + map，数据竞争是真实风险，不是洁癖。

## 制品

- 镜像：`ghcr.io/<org>/game-backend:<git-sha>`，`latest` 只在 main 成功时打。
- 镜像必须能 `docker run` 后 `/healthz` 200（Dockerfile 里 `HEALTHCHECK` 指向它）。
- 不把 `api/openapi/*` 之外的生成物提交进仓库。

**现状**：以上是目标，不是事实。仓库里没有 registry，`ci.yml` 只镜像 `verify`
的五个步骤、**不构建镜像**；`Dockerfile` 也从未被 `docker build` 过。
第一次真正构建与运行按 `docs/deploy.md` 走，别假设 CI 产出过镜像。

## 环境与升级

| 环境 | 用途 | 规则 |
|---|---|---|
| dev | 本地/共享开发 | 可用 dev auth，可内存存储 |
| staging | 预发验收 | 关闭 dev auth，接真实 Nakama 与真实托管服 |
| prod | 线上 | 关闭 dev auth，服务 token 来自 secret manager |

关闭 dev auth 的前提是**有真实认证器**（Nakama JWT 校验，M3）。在那之前
`BACKEND_DEV_AUTH=false` 会让平台拒绝启动——这是有意的硬失败，不是 bug：
宁可起不来，也不要拿 dev bearer token 上预发。

Agent 边界：**不得**执行任何 staging/prod 部署命令。只允许本地构建与本地运行。

## 数据库迁移（接入 Postgres 之后）

- 迁移文件 `db/migrations/NNNN_<slug>.sql`，只增不改（已发布的迁移不可编辑）。
- 前向兼容三步走：加字段（nullable）→ 双写 → 回填 → 切读 → 删旧字段。
- 破坏性变更必须能回滚；回滚方案写进 spec 的"回滚"一节。
- 迁移在部署前独立执行，不做"应用启动自动迁移"。
- 迁移必须配探针：`db/probes/*.probe.sql` 断言"该拒的必须被拒"。本机用
  `scripts/pg-local.cmd -Action probe` 跑，CI 里也跑（见下）。
- `services/*/store_equiv_test.go` 在无 `BACKEND_TEST_DATABASE_URL` 时 **skip**。
  本机直接 `go test ./...` 会看到 skip——那是正常的，别把"本机绿"当成 SQL 路径跑过。
- **CI 有 Postgres**：workflow 起 `postgres:16` service，依次 `migrate` →
  `schema probes` → `go test -race ./...`（带 `BACKEND_TEST_DATABASE_URL`）。
  所以 CI 绿 = race 跑过 + 七个等价性测试在真库上跑过 + 约束探针通过。
- CI 还会**断言这些测试没有 skip**（`assert the database suites did not skip`）：
  跳过的测试和通过的测试在汇总行里长得一样，所以这一步会 grep `SKIP:` 并让构建失败。
  取消这条检查等于取消整条数据库验证。

## 发布检查单

- [ ] `verify` 全绿且 `apigen -check` 通过
- [ ] 契约变更已在 PR 描述里列出（op id 级别）
- [ ] 新 op 有 e2e 覆盖
- [ ] 新增环境变量已在 `pkg/config` 与文档中登记
- [ ] 回滚方案明确（上一个镜像 tag + 数据兼容性）
- [ ] 没有新增裸密钥
- [ ] 镜像 tag 是 git sha 或语义版本，不是 `latest`
- [ ] 目标环境 `/healthz` 与 `/readyz` 都返回 200（`/metrics` 有请求计数）
