# 0008 - 每场对局的运行时凭证（DS 双凭证）

- 状态：implemented
- 日期：2026-01-01
- 关联 ADR：0002（DS 是局内唯一权威）、0007（结算事务）
- 涉及：`services/session`、`pkg/battlealloc`、`tools/fakeds`、e2e、契约

## 1. 背景

`session.mark_ready` 与 `session.submit_result` 之前只认**共享** service token。
后果：任何持有该 token 的进程（被攻破的一台 DS、任何能读到环境变量的组件）都能给
**任意一场**对局上报战报。共享密钥能证明"某个信任的进程在说话"，证明不了"是这一场
的 DS 在说话"——而后者才是"DS 是局内唯一权威"这句话的实际含义。

## 2. 目标 / 非目标

目标：

1. 每场会话一个随机凭证，只有该场的 DS 拿到（通过厂商 env 注入）。
2. 两个上报接口要求**双凭证**：service token（进程身份）+ `X-Session-Token`（会话身份）。
3. 凭证不泄漏给玩家：`session.get` 永远不返回它。
4. 不改任何 op 的 Auth 级别（仍是 `AuthService`），只在 handler 层加一道会话校验。

非目标：

- 不替换 service token 模型（其它 service 调用仍用它）。
- 不做凭证轮换/吊销列表（会话结束即失效，没有续期概念）。
- 不覆盖客户端侧凭证（客户端不进 DS 的上报路径）。

## 3. 设计

```text
session.create（service 调用）
  ├─ mint 32B 随机 token（base64url）
  ├─ 写 sessions.runtime_token（0001 已建列）
  ├─ 传给 Allocator → 厂商把它注入实例环境（Edgegap env_vars, is_hidden）
  └─ 响应顶层返回 runtime_token（仅此一处；玩家可读的 get 不含）

DS 上报
  POST /v1/session/sessions/{id}/ready    Authorization: Bearer <service> + X-Session-Token
  POST /v1/session/sessions/{id}/results  同上
       ↓
  handler：先比会话凭证（常量时间），再走状态机
```

要点：

- **先比凭证再走状态机**：未认证的调用方不该从错误码里学到会话处于什么状态。
- **失败关闭**：会话没有 `runtime_token`（旧数据/损坏行）一律 401，而不是放行。
- `session.get` 读出时把字段清零，`omitempty` 保证玩家侧 JSON 里根本没有这个键。
- 凭证在 `session.create` 的响应顶层，不在 `Session` DTO 里，避免任何"顺手带出去"。

## 4. 失败模式

| 场景 | 响应 |
|---|---|
| 只有 service token，没有 `X-Session-Token` | 401 `unauthenticated` |
| 会话 token 不匹配（别的场的 token） | 401 |
| 会话无 token（旧数据） | 401，提示重建会话 |
| 凭证正确但状态非法（如 failed 后 ready） | 412 `failed_precondition`（状态机原样） |
| 玩家 `GET /sessions/{id}` | 200，响应里没有 `runtime_token` |

## 5. 验证

| 层 | 用例 |
|---|---|
| e2e | 完整对局带双凭证跑通；**不带会话 token 上报 → 401** |
| 服务 | 9 个既有用例全部改带 header；分配失败后：无 token → 401、有 token → 412 |
| 存储 | 等价性测试断言 `runtime_token` 落库、并在 Update 后保留（内存与 SQL 一致） |
| 适配器 | 契约测试断言 Edgegap 的 `env_vars` 里有 `RUNTIME_TOKEN` 且 `is_hidden` |
| 契约 | `apigen -check` 无漂移（新增字段与两个 op 的 Note 已发布） |

## 6. 回滚

代码回退即恢复"只认 service token"。`sessions.runtime_token` 列留着无害（可空）。

## 7. 未决

- [ ] 真机验证：Edgegap 冒烟时确认 DS 真的读到了 `RUNTIME_TOKEN`（阻塞于套餐并发额度）
- [ ] 是否给 `session.create` 也加幂等键（现在靠 `sessions_one_per_match` 拦重复）