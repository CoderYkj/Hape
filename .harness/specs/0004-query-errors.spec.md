# 0004 - 两个查询不再静默降级

- 状态：implemented
- 日期：2026-01-01
- 关联 ADR：0006（第 2 条已知债务）
- 涉及：`rank.Store.RatingOf`、`matchmaking.Store.FindWaitingByPlayer`、
  `matchmaking.RatingLookup`、`rank.Service.Rating`

## 1. 背景

ADR 0006 第 2 条债：两个高频查询没有 `error` 返回，数据库出错时静默降级。

```go
// 之前
RatingOf(ctx, board, playerID) int64                       // 出错 → 返回 1000
FindWaitingByPlayer(ctx, queue, playerID) (Ticket, bool)    // 出错 → 当成"没在排队"
```

后果不是"少个字段"，而是**两类安静的错误**：

1. 排队的 MMR 被当成 1000 → 匹配质量悄悄变差，事后无从追溯。
2. "没在排队" → 服务去创建第二张票 → 撞部分唯一索引 → 玩家收到一个莫名其妙的 409。

两条都属于"出错了但看起来像正常"，比 500 难查得多。

## 2. 目标 / 非目标

目标：

1. 两个查询都返回 `error`；调用方把失败变成 `503 unavailable`（可重试），
   而不是替玩家做决定。
2. "记录不存在"仍然是正常情况：没打过排位 → 返回 `DefaultRating`；
   没在排队 → `found=false, err=nil`。
3. 失败路径有测试：连接池关闭后两个查询必须返回错误。

非目标：

- 不改对外契约（错误码仍是既有的 `unavailable`）。
- 不改匹配规则、评分算法。
- 不做重试/熔断（等有真实流量再谈）。

## 3. 签名变化

| 位置 | 之前 | 之后 |
|---|---|---|
| `rank.Store` | `RatingOf(ctx, board, playerID) int64` | `RatingOf(ctx, board, playerID) (int64, error)` |
| `rank.Service` | `Rating(ctx, playerID) int64` | `Rating(ctx, playerID) (int64, error)` |
| `matchmaking.Store` | `FindWaitingByPlayer(ctx, queue, playerID) (Ticket, bool)` | 同上 + `error` |
| `matchmaking.RatingLookup` | `func(ctx, playerID) int64` | `func(ctx, playerID) (int64, error)` |

## 4. 失败模式

| 场景 | 之前 | 之后 |
|---|---|---|
| 评分查询失败（入队） | 按 1000 分入队 | `503 unavailable`，不入队 |
| 排队查询失败（入队） | 当成没在排队，尝试建第二张票 | `503 unavailable`，不建票 |
| 玩家从未打过排位 | 1000 | 1000（`ErrNoRows` 视为正常） |
| 玩家确实没在排队 | `found=false` | `found=false, err=nil` |

## 5. 验证

| 层 | 用例 |
|---|---|
| 服务 | `TestEnqueueFailsWhenTheRatingLookupFails`：注入失败的 `RatingLookup` → 503 + `unavailable` |
| 服务 | `TestEnqueueFailsWhenTheQueueLookupFails`：注入失败的 store → 503 |
| 服务 | `TestEnqueueStillWorksWhenEverythingIsHealthy`：正常路径仍 200，MMR 如实写入 |
| 存储 | `TestPGReportsLookupErrors` / `TestPGReportsQueueLookupErrors`：关掉连接池后必须报错 |
| 回归 | 七个等价性测试 + e2e 全绿（含真库两轮） |

## 6. 回滚

纯签名与错误处理改动，无数据变更。回滚 = revert。

## 7. 未决

- [ ] 入队时评分服务不可用，是否该降级为"允许排队但标记待定 MMR"（产品决策，不是技术决策）