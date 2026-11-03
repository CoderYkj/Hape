# 30 - 接口契约

## 唯一来源

契约只有一个来源：**Go 代码里的 `api.Op` 声明 + DTO struct**。
`api/openapi/*.json` 是 `go run ./tools/apigen` 的产物。

```text
api.Op{...} + DTO struct  ──apigen──▶  api/openapi/<service>.json
        │
        └─ 同一个 Op 同时决定路由注册（method + path）
```

因此下列事情**不可能**发生：路由和文档不一致、文档漏了字段、字段类型对不上。

- 改契约 = 改 Go 声明 + `go run ./tools/apigen`。
- 手改 `api/openapi/*.json` 会被 `apigen -check` 判失败，属违规。
- 字段说明写 struct tag：`json:"x" doc:"人话"`；`omitempty` 即"可选"。

## 命名

- op id：`<service>.<verb>`，全小写蛇形，如 `session.submit_result`。这个 id 就是 OpenAPI 的 `operationId`。
- 路径：`/v1/<service>/<资源复数>[/{id}][/<子资源>]`，如 `/v1/economy/players/{player_id}/wallet`。
- 路径参数用 `{snake_case}`（既是 `http.ServeMux` 通配符，也是合法 OpenAPI path template）。
- 请求/响应体：`<Verb><Resource>Request` / `<Verb><Resource>Response`，包一层命名对象，不要裸数组。
- 查询参数：写在 `api.Op.Query`（`Name` snake_case、类型只允许 `integer`/`string`/`boolean`、必须写 `Doc`），
  `apigen` 会渲染成 `in: query`；不在 handler 里偷偷读一个没声明的参数。
- 翻页一律**游标**（`after` + 响应里的 `next_after`），不用 offset：只追加的表用 offset 会重复/漏行。

## 在线格式

- JSON，字段名 `snake_case`。
- 时间：RFC3339 UTC，`"2026-01-01T00:00:00Z"`。
- 金额/XP/评分：**整数**。货币字段必须带 `currency`，不接受隐含单位。
- 枚举用字符串（`"allocating"`），不用魔法数字。
- 不返回内部字段（DB id、租约、内部错误栈）。

## 错误模型

唯一形状：

```json
{"error":{"code":"not_found","message":"player not found","details":{"player_id":"p_9"}}}
```

`code` 与 HTTP 状态一一对应（`pkg/apierr` 是唯一实现）：

| code | HTTP | 用在哪 |
|---|---|---|
| `invalid_argument` | 400 | 字段缺失/格式错/未知字段 |
| `unauthenticated` | 401 | 无 token / token 无效 |
| `permission_denied` | 403 | 有身份但无权（读别人钱包） |
| `not_found` | 404 | 资源不存在 |
| `conflict` | 409 | 状态冲突（同一 session 上报了不同 result） |
| `failed_precondition` | 412 | 余额不足、队伍已满 |
| `resource_exhausted` | 429 | 限流 |
| `unavailable` | 503 | 下游（战斗服分配/扇出）失败，可重试 |
| `internal` | 500 | 未预期错误，message 固定 |

规则：**一个 code 一个状态，永远不改**。新增 code 需要 ADR。

## 幂等

- 写操作的 `api.Op` 必须二选一：
  - `Idempotent: true` —— 同一个 `Idempotency-Key` 重放返回 `applied: false`，不产生副作用；
  - `Idempotent: false` + `Note` 说明为什么不可重放、调用方该如何重试。
- `platform/contract_test.go` 会把"既没声明幂等、也没写 Note 的写操作"判失败。
- 需要 `Idempotency-Key` 的 op，缺 header 返回 400 `invalid_argument`（不是 401/500）。
- 幂等键作用域：`(player_id, key)` 或 `(session_id, result_id)`，写清在 Note 里。

## 校验

- 请求体用 `httpx.Decode`：**拒绝未知字段**（`DisallowUnknownFields`），拼错参数是 400 而不是静默忽略。
- 长度上限：显示名 24 rune、队伍名 32 rune、`Idempotency-Key` 128 字符、请求体 1 MiB。
- 分页/限量：任何返回集合的 op 必须有硬上限（v0 排行 50）。

## 版本演进

- 破坏性变更（删字段、改语义、改认证级别）→ 新路径 `/v2/...` + ADR，旧路径保留至少一个发布周期。
- 非破坏性变更（加可选字段、加新 op）→ 直接加，`apigen` 重新生成。
- 弃用：先在 `Note` 标注，再在 `docs/api-contract.md` 记录移除时间。

## 生成与门禁

```bash
go run ./tools/apigen          # 写出 api/openapi/*.json
go run ./tools/apigen -check   # CI/verify 用：内容不一致即失败
```

`apigen -check` 之前还会跑 `api.Validate(ops)`：op id 重复、同 method+path 冲突、缺 Summary、非法 Auth 都会直接失败。
