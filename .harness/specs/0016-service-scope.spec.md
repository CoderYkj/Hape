# 0016 - 服务凭证最小权限（scope）

- 状态：implemented
- 日期：2026-11-01
- 关联 ADR：0010（登录收口）；前置 spec 0014（凭证轮换）
- 涉及：`pkg/httpx`（`ServiceCredential.Services`、Router 执行点）、`pkg/config`、`platform`、`deploy/*`

## 1. 背景

spec 0014 解决了"凭证换不掉"，没解决"一条凭证能调所有服务接口"。当时的量化缺口是：

- 服务间接口共 **8 个 op、6 个域**：`account.login`、`economy.grant`、`matchmaking.pop`、
  `progress.grant_xp`、`rank.submit_result`、`session.create`、`session.mark_ready`、
  `session.submit_result`。
- 一条泄露的凭证因此可以**凭空造货币**（`economy.grant`）、**不断分配实例烧配额**
  （`session.create`）——是钱和配额，不是积分榜。
- 伪造战报本来就被每场 `X-Session-Token` 挡住了（spec 0008），所以这里的边界是"钱"。

轮换只能限时止损；限范围要靠 scope。

## 2. 目标 / 非目标

目标：

1. 每条凭证声明它能调哪些**服务**；不在列表里 → `403 permission_denied`。
2. **默认拒绝**：没写 scope = 什么都不能调（fail closed），不是"什么都能调"。
3. 启动期校验：scope 里写了本进程没有的 service → 直接拒绝启动（拼错的 scope 永远不会匹配，
   等于静默失效，最难查）。
4. 观察模式：`BACKEND_SERVICE_SCOPE_ENFORCE=false` 时只记 `would-deny` 日志、不拦截，
   用来拿真实流量验证策略。
5. 一条覆盖性测试：仓库里付运的示例 env 必须能触达**每一个** service-auth op。

非目标：

- 不做 op 级 ACL（现在到 service 级，粒度够用；`op.Service` 是天然的边界）。
- 不做 mTLS / 短期凭证 / 实例身份（见 §9）。
- 不改玩家侧鉴权（`AuthPlayer` 的 op 与 scope 无关）。

## 3. 语法

```text
BACKEND_SERVICE_CREDENTIALS=nakama=<tok>:account,ops=<tok2>:*,worker=<tok3>:matchmaking+session
```

| 形式 | 含义 |
|---|---|
| `name=token` | 能认证，**不能调任何 service**（配置错误，启动日志会点名 scope=none） |
| `name=token:account` | 只能调 `account` 域 |
| `name=token:a+b` | 只能调 `a` 和 `b` |
| `name=token:*` | 全部（运维工具显式使用；`*` 不能和其它混写） |

约束：

- token 不允许含 `,` 或 `:`（语法分隔符；本仓库的凭证是 hex/base64url，都不含）。
- 名字 `^[a-z][a-z0-9_-]{0,31}$`，service 名 `^[a-z][a-z0-9_]{0,31}$`。
- 单条凭证的 service 不允许重复；`*` 不允许与具名 service 并列。

## 4. 执行点

```text
请求 → Router 中间件 → 认证（得到 Principal{Service: 凭证名, Scopes: [...]}）
                    → AuthService 的 op？ → scopeAllows(caller, op.Service)
                          是  → 继续
                          否  → 403 permission_denied{caller, service}（enforce）
                          否  → Warn "service credential out of scope (not enforced)"（observe）
```

三个设计判断：

- **在 Router 而不是在各 service 里**：这是唯一所有 op 都经过的地方，漏一个 handler 就是漏一个洞。
- **403 而不是 401**：凭证是真的，只是不允许做这件事。401 会让运维去查"token 是不是过期了"，
  403 才会让人去看 scope。
- **fail closed**：权限检查唯一安全的默认是拒绝。配合 observe 模式，新编码不会立刻炸。

## 5. 落地顺序（观察期 → 强制）

```bash
# 1) 先开观察模式：策略写错也不影响服务，只是日志里出现 would-deny
BACKEND_SERVICE_SCOPE_ENFORCE=false
# 2) 部署，跑 24h，检查有没有越权调用
#    日志过滤：msg="service credential out of scope (not enforced)"
# 3) would-deny 归零 → 把开关切回 true，再部署
BACKEND_SERVICE_SCOPE_ENFORCE=true
# 4) 抽查一个越权调用：应返回 403 permission_denied（而不是 401）
```

判据是**指标**而不是日志：观察模式累加 `backend_service_scope_would_deny_total`，切强制后
该计数应恒为 0。两个计数器在启动时以 0 注册（`backend_service_scope_denied_total` 同理），
所以"归零"是可查询的事实，不会和"指标没注册"混淆。日志行仍带
`caller`/`op`/`op_service`/`scopes` 四个字段，用来定位要改哪条配置。

## 6. 失败模式

| 场景 | 结果 |
|---|---|
| 凭证没写 scope | 认证通过，所有 service-auth op 403（观察模式只记日志） |
| scope 写了不存在的 service | **启动失败**，报出 `凭证:service` 与已知 service 列表 |
| `*` 与具名 service 混写 | 解析失败，启动失败 |
| token 含 `:` 或 `,` | 解析失败，启动失败 |
| 凭证不在集合里 | 401（与之前一致） |
| 玩家 token 调 service op | 401（与之前一致） |
| 旧变量 `BACKEND_SERVICE_TOKEN` | 包成 `service`，scope `*`（变量本身无法表达 scope；迁移到新变量才有最小权限） |
| 观察模式下越权 | 200，日志 `would deny` |

## 7. 验证

| 层 | 用例 |
|---|---|
| 解析 | `a=t:acct`、`a=t:a+b`、`a=t:*`、省略 scope；`*+a`、空 scope 项、非法 service 名、重复 service、token 含 `:` 全部拒绝 |
| 语义 | `Allows()`：具名/通配/空 scope 三态；`UnknownServices()` 报出拼错的 scope |
| 端到端（平台） | 限 `account` 的凭证：`account.login` 200、`session.create` 403 且 code=`permission_denied`；同凭证在观察模式下 `session.create` 200 |
| 覆盖性 | 解析 `deploy/systemd/backend.env.example` 的凭证行：每个 service-auth op 至少被一条凭证覆盖，且不含未知 service |
| 指标 | 强制模式越权 → `backend_service_scope_denied_total` +1；观察模式 → `would_deny_total` +1 且 denied 不动；两个计数在启动后以 0 可见 |
| 回归 | 玩家 token 仍不能调 service op；旧单 token 变量仍能全通（scope `*`） |

## 8. 回滚

- 最小改动：`BACKEND_SERVICE_SCOPE_ENFORCE=false`（观察模式，不返回 403）。
- 彻底回退：代码回退即恢复"只看 token 不看 scope"。

## 9. 未决

- [ ] op 级 ACL（例如 `nakama` 只能调 `account.login` 而不能调同域的其它 op）。现在到 service 级；
      将来 `account` 域只剩一个 service op 时自然就等价了。
- [ ] mTLS 或短期凭证：scope 解决"能做什么"，不解决"凭证长期有效"。
- [ ] 越权的告警：现在只有日志，没有"某凭证今天被拒 N 次"的指标。
- [ ] 实例级身份（每台 DS 一个短期凭证而不是共享 service token）——更大的改动，属 M4。