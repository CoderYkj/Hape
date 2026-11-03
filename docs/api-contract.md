# 契约工作流

> 读者：要改接口的人 · 状态：活文档 · 最后更新：2026-11-03 · 上游：rules/30 + `api/openapi/*.json`（生成物）

## 1. 契约从哪来

```text
services/<域>/service.go
    api.Op{ID, Method, Path, Request: api.Body(XRequest{}), Response: api.T(XResponse{}), ...}
    + DTO struct（json tag 决定字段名，doc tag 决定描述，omitempty 决定可选）
                │
                ▼  go run ./tools/apigen
        api/openapi/<service>.json        （OpenAPI 3.1，生成物，勿手改）
```

同一个 `api.Op` 同时决定：

- 路由：`router.Handle` 用 `op.Method + " " + op.Path` 注册到 `http.ServeMux`；
- 文档：apigen 用它生成 OpenAPI；
- 测试门禁：`platform/contract_test.go` 校验合法性与重试语义。

所以"文档与实现不一致"在结构上不可能发生。

## 2. 加一个接口的完整步骤

```go
// services/team/service.go

// 1) DTO：字段名即 JSON 名，doc 即文档描述
type ListTeamsResponse struct {
    Teams []Team `json:"teams" doc:"当前玩家所在的队伍，最多 20 条。"`
}

// 2) 注册到 r，声明契约
r.Handle(api.Op{
    ID:       "team.list",
    Service:  "team",
    Method:   http.MethodGet,
    Path:     "/v1/team/teams",
    Summary:  "List the caller's teams",
    Response: api.T(ListTeamsResponse{}),
    Auth:     api.AuthPlayer,
}, s.list)

// 3) handler：返回响应体或 apierr
func (s *Service) list(r *http.Request) (any, error) { … }
```

```bash
go run ./tools/apigen        # 生成 api/openapi/team.json
go test ./...                # 契约测试 + e2e
```

不要忘记：新 op 要有 e2e 用例（`.harness/rules/40-testing.md`）。

## 3. 字段规则速查

| Go 类型 | JSON Schema | 备注 |
|---|---|---|
| `string` | `{"type":"string"}` | 长度上限在 handler 里校验 |
| `time.Time` | `string/date-time` | 永远 UTC |
| `int64` | `integer/int64` | 钱、XP、评分 |
| `int32` | `integer/int32` | 等级、序号 |
| `bool` | `boolean` | 用肯定式命名 |
| `[]string` | `array of string` | 集合要有上限 |
| `[]T`（struct） | `array of $ref T` | |
| `map[string]string` | `object` + `additionalProperties` | 钱包余额用这个 |
| `*time.Time` | `anyOf[$ref, null]` | 可空时间 |

### 3.1 查询参数

查询参数和 path 参数一样属于契约：在 `api.Op` 的 `Query` 里声明，`apigen` 渲染成 OpenAPI
的 `in: query` 参数，`api.Validate` 要求名字是 snake_case、类型只能是 `integer`/`string`/
`boolean`、每个参数必须写 `Doc`。

当前唯一使用方是 `economy.get_ledger`：

| 参数 | 类型 | 说明 |
|---|---|---|
| `limit` | integer | 页大小 1–200，缺省 50；超过上限按上限截断（不报错） |
| `after` | string | 上一页响应里的 `next_after`；省略表示第一页 |

**用游标不用偏移**：账本只追加，用 offset 时两页之间新入账一笔会让后面的行整体错位，
客户端就会重复一行、漏掉一行。游标不存在或不属于该玩家时返回 400，而不是悄悄回到第一页。

## 4. 消费契约

- **运营/调试**：`curl` 直接打（`api/openapi/*.json` 里有完整字段）。
- **TS 客户端**：`npx openapi-typescript api/openapi/*.json`（未在本仓库验证）。
- **C++/UE5**：按 `docs/ue5-ds-integration.md` 手写 3 个调用，不值得为此引代码生成。
- **未来 gRPC**：同一份 DTO 生成 proto（ADR 0004 记录了为什么现在不做）。

## 5. 门禁

```bash
go run ./tools/apigen -check   # 内容与生成物不一致 → 非零退出
```

`verify` 已包含它。手改 `api/openapi/*.json` 一定会被这条拦住。

## 6. 版本与破坏性变更

- 加字段、加 op：直接加，重新生成。
- 删字段、改语义、改认证级别：新路径 `/v2/...` + ADR，旧路径保留一个发布周期。
- `api.Validate` 会拦住：op id 重复、同 method+path 冲突、缺 Summary、非法 Auth 值。
