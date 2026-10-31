# 20 - Go 代码规范

## 包布局与依赖方向

```text
cmd/*            main：读配置 → 组装 → httpx.Run。不允许出现业务逻辑
platform/*       装配：创建服务、注入依赖、挂路由、比赛结果扇出
services/<域>/*  领域：类型、Store 接口、契约声明、handler、领域方法
pkg/api         契约原语（Op/Shape/OpenAPI）。只依赖标准库
pkg/apierr      错误模型。只依赖标准库
pkg/httpx       传输层：路由、鉴权、解码、指标。可被 services 使用
pkg/battlealloc 战斗服分配抽象。不 import 任何 services
tools/*          一次性工具（apigen、fakeds）
```

硬规则：

- `pkg/*` **不得** import `services/*` 或 `platform/*`。
- `services/a` 不得 import `services/b`。跨域协作只在 `platform` 里接线（见 `platform/sink.go`）。
- 依赖方向永远单向：`cmd → platform → services → pkg`。

## 错误处理

- 业务错误一律 `apierr.*`（`apierr.NotFound` / `Invalid` / `Precondition` ...）。
- 包装底层错误用 `.WithCause(err)`；**客户端只看到 Message，内部细节只进日志**。
- 未知错误由 `apierr.As` 归一成 `internal`，message 固定为 `internal error`。
- 禁止 `panic` 表达可预期失败；`panic` 只留给编程错误，`httpx` 会兜底成 500 并记栈。
- 禁止 `_ = err` 吞错；确实要忽略时注释原因。
- 错误字符串小写、无标点结尾（`"player not found"`）。

## context

- 每个 handler 的第一个参数是 `*http.Request`，从 `r.Context()` 取 context。
- 向下传递 ctx；**不得**在 handler 里用 `context.Background()`。
- 跨服务调用必须带超时（v0 进程内调用无需，拆服务时必须加）。
- 不使用 context 传递业务参数（用函数参数）。

## 并发

- 共享可变状态必须加锁，且锁要覆盖"读-改-写"整体（见 `Memory.Grant`）。
- 临界区里不做 I/O、不调用外部服务、不写日志。
- 任何启动的 goroutine 必须能被 ctx 取消；`httpx.Run` 是唯一允许常驻的地方。
- 定时任务（未来 matchmaker 循环）必须单飞：同一队列只允许一个 pop 者（见 `matchmaking.pop` 的 Note）。

## 时间与随机

- 所有对外时间戳 UTC、RFC3339。
- 需要可测的时间，走 `now func() time.Time` 字段（见各 `Memory`），不要在业务里直接 `time.Now()`。
- 测试里禁止 `time.Sleep` 等待（唯一例外：`tools/fakeds` 演示脚本）。

## 命名

- 接口名不带 `I` 前缀；单方法接口用 `-er` 后缀（`Allocator`、`Store`）。
- 构造函数 `New` / `NewWithStore`；后者用于测试注入。
- 布尔字段用肯定式（`Applied`，不是 `NotApplied`）。
- 包名短、全小写、无下划线（`apierr`、`battlealloc`、`httpx`）。

## 依赖政策

- **依赖最小化**。当前唯一第三方依赖是 `github.com/jackc/pgx/v5`（ADR 0006，仅出现在 `pkg/pg` 与 `services/*/store_pg.go`）。新增任何依赖必须先在 `.harness/adr/` 写一篇 ADR，说明：解决什么问题、维护状态、退出成本。
- 不引入 ORM、DI 框架、web 框架；`net/http` + `log/slog` + 手写装配足够。

## 存储

- 每个域一个 `Store` 接口 + `Memory` 实现；handler 只依赖接口。
- `Memory` 的方法必须是并发安全且语义完整的（幂等、校验、拒绝）。
- SQL 实现落地时不得改接口签名；需要改就说明理由并更新 spec。
- 金额只用 `int64`，单位文档化在字段 `doc` 里。**永远不用 float 表示钱。**

## 日志

- 用 `slog`，字段化，禁止 `fmt.Printf` 打业务日志。
- 一行一个事件；关键字段：`request_id`、`op`、`player_id`、`result_id`。
- 不打 token、不打完整请求体、不打 PII。

## 反例清单（见到就改）

```go
panic(err)                          // 可预期错误
log.Println("player", id)           // 非结构化
if err != nil { return nil }        // 吞错
go func(){ ... }()                  // 无 ctx、无法停止
mu.Lock(); defer mu.Unlock()        // ok，但临界区内若有 I/O 就是错的
time.Sleep(100 * time.Millisecond)  // 当同步手段用
float64 表示余额                      // 违反经济不变量
```
