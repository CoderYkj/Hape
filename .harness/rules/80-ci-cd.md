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

`-race` 在 CI 必开：这个后端到处是 `sync.Mutex` + map，数据竞争是真实风险，不是洁癖。

## 制品

- 镜像：`ghcr.io/<org>/game-backend:<git-sha>`，`latest` 只在 main 成功时打。
- 镜像必须能 `docker run` 后 `/healthz` 200（Dockerfile 里 `HEALTHCHECK` 指向它）。
- 不把 `api/openapi/*` 之外的生成物提交进仓库。

## 环境与升级

| 环境 | 用途 | 规则 |
|---|---|---|
| dev | 本地/共享开发 | 可用 dev auth，可内存存储 |
| staging | 预发验收 | 关闭 dev auth，接真实 Nakama 与真实托管服 |
| prod | 线上 | 关闭 dev auth，服务 token 来自 secret manager |

Agent 边界：**不得**执行任何 staging/prod 部署命令。只允许本地构建与本地运行。

## 数据库迁移（接入 Postgres 之后）

- 迁移文件 `db/migrations/NNNN_<slug>.sql`，只增不改（已发布的迁移不可编辑）。
- 前向兼容三步走：加字段（nullable）→ 双写 → 回填 → 切读 → 删旧字段。
- 破坏性变更必须能回滚；回滚方案写进 spec 的"回滚"一节。
- 迁移在部署前独立执行，不做"应用启动自动迁移"。

## 发布检查单

- [ ] `verify` 全绿且 `apigen -check` 通过
- [ ] 契约变更已在 PR 描述里列出（op id 级别）
- [ ] 新 op 有 e2e 覆盖
- [ ] 新增环境变量已在 `pkg/config` 与文档中登记
- [ ] 回滚方案明确（上一个镜像 tag + 数据兼容性）
- [ ] 没有新增裸密钥
