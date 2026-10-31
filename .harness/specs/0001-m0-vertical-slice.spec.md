# 0001 - M0 垂直切片：一场对局跑通全链路

- 状态：implemented
- 日期：2026-01-01
- 关联 ADR：0001、0002、0003、0004、0005
- 涉及服务：account、team、matchmaking、session、progress、economy、rank

## 1. 背景

新团队最常见的失败模式：先把"架构"画得很大，然后半年没有一局能完整结算。
M0 的目标不是功能齐全，而是**证明这条链是通的、可测的、可回滚的**：

```text
登录 → 组队 → 排队 → 匹配 → 分配战斗服 → DS 就绪 → 上报战报 → 进度/经济/排行更新
```

## 2. 目标 / 非目标

目标：

1. 单进程 `go run ./cmd/platform` 即可跑完整链路，零外部依赖。
2. `go test ./...` 覆盖整条链路（含拒绝分支），CI 可复现。
3. 契约有唯一来源，`apigen -check` 能拦住漂移。

非目标：

- 真实 Postgres 持久化（v0 内存，接口已留）。
- 真实托管战斗服接入（只有 `local` 与三个 `Unconfigured` 占位）。
- Nakama 侧社交功能落地（只有插件骨架）。
- 反作弊、掉线重连、观战、回放。

## 3. 用户故事与验收标准

```text
Given 后端以 dev 配置启动
When  两名玩家登录并各自 enqueue ranked_1v1
Then  pop 返回 matched=true 与同一个 match_id

Given 一个已创建、已 ready 的 session
When  Dedicated Server 上报一份战报（result_id=X）
Then  session 变 finished，胜者金币 +100、XP +50+score/10、rating +24

Given 同一份战报（result_id=X）再次上报
Then  返回 applied=false，余额/XP/rating 均不变

Given 客户端（玩家 token）尝试上报战报
Then  返回 401 unauthenticated
```

以上全部由 `e2e/flow_test.go` 的 `TestMatchLifecycle` 断言。

## 4. 契约变更

M0 建立 21 个 op，分属 7 个服务（见 `api/openapi/*.json`）。摘要：

| 服务 | ops |
|---|---|
| account | `login`、`get_player`、`update_profile` |
| team | `create`、`join`、`get` |
| matchmaking | `create_ticket`、`get_ticket`、`pop` |
| session | `create`、`mark_ready`、`submit_result`、`get` |
| progress | `get`、`grant_xp` |
| economy | `get_wallet`、`grant`、`purchase`、`get_ledger` |
| rank | `get_leaderboard`、`submit_result` |

## 5. 数据模型变更

内存实现：`services/*/store.go`。字段与 `api.T(...)` 反射出的 schema 一致。
SQL schema 见 `db/migrations/0001_init.sql`：已在 Postgres 16.4 上执行，
约束由 `db/probes/0001_constraints.probe.sql` 断言（35 条全 PASS）。
M0 时点 Go 侧仍是内存实现；**M1 已完成**（7/7 域落库，见 spec 0002 / ADR 0006）。

## 6. 不变量与失败模式

不变量：

- 余额非负；货币白名单 `gold`/`gems`；金额为正整数。
- 一个 session 只接受一份 result；第二个不同 result → 409。
- 战报 player 集合 === session player 集合。
- 状态机只走 `session.CanTransition` 允许的边。

失败模式：

| 场景 | 响应 |
|---|---|
| 分配失败 | 503 + session 置 `failed` |
| 扇出失败 | 503 + session 保持 `ready/in_progress`，可重试 |
| 非法状态跳转 | 412 `failed_precondition` |
| 余额不足 | 412 `failed_precondition`（带 balance/required） |
| 缺 Idempotency-Key | 400 `invalid_argument` |

## 7. 可观测性

访问日志一请求一行；结算链路日志带 `session_id`、`result_id`、`applied`
（首次落地 `applied=true`，重放 `applied=false`，两者都有一行）。
`/metrics` 暴露 `backend_requests_total{service,op,status}`。

## 8. 测试计划

| 层 | 用例 |
|---|---|
| 单元 | `pkg/api`：形状反射、路径参数、重复路由校验 |
| 契约 | `platform`：op 合法、7 个服务都有 op、写操作声明重试语义、session 写操作必须 service auth |
| e2e | 完整闭环；匿名读、越权读、客户端伪造战报、重复上报、冲突上报、余额不足、未知字段 |
| 失败模式 | `services/session`：分配失败→503+会话 failed、扇出失败→503+会话保持 ready 且可重试、战报玩家集合必须与名单一致、非参战者读会话→403、名单校验（<2 / 重复 / 空）、结算后 `Release` 恰好一次且重放不重复释放、超时会话被扫成 failed 并 `Release` |
| 装配 | `platform`：dev auth 关闭且未注入认证器→拒绝启动、注入后可启动、reconciler 关闭时立即返回、开启时把超时会话扫成 failed |
| 配置 | `pkg/config`：默认值、duration 解析、格式错误即报错、prod+dev auth 拒绝、开启清理但阈值为 0 拒绝 |

## 9. 回滚方案

v0 无持久化，回滚 = 回退镜像。无数据迁移。

## 10. 未决问题

- [ ] DS 与后端的 per-allocation token 何时落地（见 ADR 0003）
- [x] 僵尸会话对账由谁调度 → 已定：`platform.RunReconciler` 用进程内 ticker，
      `BACKEND_RECONCILE_INTERVAL` 开启（默认关）。多实例时的单活选主仍未定。
- [ ] 战报 `schema_version` 字段是否需要（UE5 版本升级时）
