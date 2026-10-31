# 40 - 测试

## 三层，缺一不可

| 层 | 位置 | 测什么 | 速度 |
|---|---|---|---|
| 单元 | `services/<域>/*_test.go`、`pkg/*_test.go` | 规则、不变量、状态机、形状反射 | 毫秒 |
| 契约 | `platform/contract_test.go` | op 合法、无路由冲突、写操作声明重试语义、认证级别 | 毫秒 |
| 端到端 | `e2e/flow_test.go` | 真实 HTTP 走完整对局闭环 + 拒绝分支 | 秒 |

## 硬要求

- **新 op 必须进 e2e**：至少一条 happy path 断言 + 一条拒绝分支（鉴权失败 / 参数非法 / 状态冲突，按语义选）。
- **新不变量必须有单测**：余额不足、重复上报、状态机非法跳转、队伍满员、重复入队。
- 修 bug 先写失败测试，再修。
- 测试必须可重复：无网络、无真实时间依赖、无固定端口（用 `httptest`）。

## 写法

- 表驱动：

```go
tests := []struct{ name string; in int64; want Code }{...}
for _, tc := range tests {
    t.Run(tc.name, func(t *testing.T) { ... })
}
```

- 断言用标准库，不引 testify：`if got != want { t.Fatalf("...") }`。
- e2e 用 `at(t, body, "session.state")` 这种点路径读断言，失败信息自带路径。
- 需要固定时间时注入 `now`，不要 mock 全局。

## 禁止

- 删除或跳过测试使其变绿（`t.Skip` 需要注释原因并开 issue 编号）。
- 无断言的测试。
- 依赖执行顺序的测试（`t.Parallel` 只在确认无共享状态时用）。
- 用 `time.Sleep` 等待异步（除 `tools/fakeds` 演示）。
- 在单测里起真实 TCP 监听。

## 运行

```bash
go test ./...                       # 全量
go test ./services/economy -v       # 单包
go test ./e2e -run TestMatchLifecycle -v
go test -race ./...                 # 并发改动后必跑
go test -count=1 ./...              # 跳过缓存
```

## 覆盖率

不设硬门槛（门槛会诱导写垃圾断言），但要求：

- 分支里出现新的 `apierr.*` 拒绝路径 → 必须有对应用例。
- e2e 里的断言必须覆盖**数值结果**（余额、XP、rating），不能只看状态码。
