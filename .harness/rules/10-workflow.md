# 10 - 工作流

## 何时必须先写 spec

满足任一条件，先写 `.harness/specs/<编号>-<slug>.spec.md`（模板见 `_TEMPLATE.spec.md`），再动代码：

- 新增或修改一个 `api.Op`（接口契约变化）
- 跨 2 个及以上 `services/*` 包
- 涉及货币、进度、排行、匹配规则
- 涉及战斗服分配或结果上报
- 预计改动超过 150 行

一行改动、修 typo、补测试不需要 spec。

## 标准流程

1. **复述任务**：目标 + 验收标准 + 不确定点（≤5 行）。
2. **读上下文**：
   - `services/<域>/service.go` 看契约声明（`api.Op`）。
   - `services/<域>/store.go` 看数据结构与不变量。
   - `platform/platform.go` 看装配与顺序。
   - `e2e/flow_test.go` 看现有闭环断言。
3. **给计划**（≤10 行）：改哪些文件、加哪些 op、加哪些测试、跑哪条验证。
4. **小步实施**：一次一个包；每步 `go build ./... && go test ./<包>`。
5. **收尾**：`pwsh -File scripts/verify.ps1`（或 `make verify`），贴输出。
6. **等人工审 diff**，不自行 commit / push / 部署。

## 计划模板

```text
目标：
验收标准：
契约变更：<op id / 无>
改动文件：
步骤：
  1.
  2.
验证：
风险：
```

## Definition of Done（缺一不可）

- [ ] `gofmt -l .` 无输出
- [ ] `go vet ./...` 通过
- [ ] `go build ./...` 通过
- [ ] `go test ./...` 通过（新逻辑有新断言）
- [ ] 新增/修改的 op 已出现在 `api/openapi/*.json`（`apigen -check` 通过）
- [ ] 新增写操作声明了 `Idempotent`，或 `Note` 说明了为什么不可重放
- [ ] e2e 覆盖了新的关键路径或拒绝分支
- [ ] 汇报中包含未验证项

## 调试流程

1. 复现：先写一个失败的测试（单元或 e2e），而不是先改代码。
2. 定位：`go test -run <Case> -v` 看失败断言；需要时 `-race`。
3. 修复：最小改动，不动无关代码。
4. 回归：`go test ./...` + `verify`。
5. 清理：删掉临时 `slog.Debug` 与调试端点。

## 何时必须停下来问

- 要改已发布的契约（路径/方法/字段语义/认证级别）。
- 要让某个 op 从 `AuthPlayer` 变 `AuthService` 或反过来。
- 要改 `Store` 接口（意味着 SQL 落点变更）。
- 要新增第三方依赖（v0 是 stdlib-only）。
- 要动 CI、部署清单、数据库迁移。
- 发现密钥、真实玩家数据、生产地址出现在仓库里。
- 任务与 `90-battle-integration.md` 的权威边界冲突。

## 汇报模板

```text
结论：<一句话>
改动：<文件列表>
契约：<新增/修改的 op id，或"无">
验证：<命令 + 关键输出>
未验证：<没有跑的东西，为什么>
风险：<回滚方式 / 影响面>
```
