# 0011 - 封闭登录接口 + 后端签发玩家 token

- 状态：implemented
- 日期：2026-01-01
- 关联 ADR：0010（决策）、0001（身份归属）、0006（JWT 认证）
- 涉及：`services/account`、`pkg/httpx`、`pkg/config`、`platform`、`e2e`

## 1. 背景

`account.login` 曾同时承担两件事，结果两件都没做好：

1. 它是 **public** 的，且信任 body 里的 `external_id`——客户端一旦直连后端，
   任何人可以冒充任意 Nakama 用户并拿到其 token；
2. 它**无条件**返回 `dev-player-<id>`，生产模式的 JWT 认证器不认，
   于是生产环境的登录链路实际是断的。

## 2. 目标 / 非目标

目标：

1. 客户端无法再调用登录接口（冒充路径关闭）。
2. 生产环境客户端能拿到**后端自己签发**、且后端能验的 token。
3. 不引入新的往返：客户端仍从 RPC 一次拿到 token。

非目标：

- 不做 token 吊销/黑名单（靠短 TTL + 重放 RPC）。
- 不做 refresh token。
- 不改 Nakama 侧任何行为。

## 3. 契约变更

| op | 之前 | 之后 |
|---|---|---|
| `account.login` | `AuthPublic`（security: none） | **`AuthService`**（security: serviceToken） |

行为变化：

- 匿名或玩家 token 调用 → `401 unauthenticated`（由路由层拒绝，handler 里再兜一次）。
- 只有持服务凭证的 Nakama 插件能调用它创建/兑换玩家。

## 4. 玩家 token

```text
iss = gamebackend        （Nakama 的会话 token 仍然是 iss=nakama）
sub = <后端 player id>
iat / exp                （默认 TTL 1h，BACKEND_PLAYER_TOKEN_TTL）
HS256，常量时间比较，exp 必填
```

`JWTAuth` 按 `iss` 分流：

| iss | 验签密钥 | 主体如何解释 |
|---|---|---|
| `nakama` | `SecurityKey` | 外部身份 → `IdentityResolver` → 后端 player id |
| `gamebackend` | `PlayerKey`（空则复用 SecurityKey） | `sub` 就是后端 player id，**不查库** |
| 其它 | — | `401 unexpected token issuer` |

## 5. 失败模式

| 场景 | 结果 |
|---|---|
| 匿名调登录 | 401（路由层） |
| 玩家 token 调登录 | 401 |
| 生产登录返回 dev token | 不可能：非 dev 模式必须注入签发器 |
| 后端 token 被篡改/换密钥 | 401（签名校验） |
| 后端 token 过期 | 401（`exp` + leeway） |
| Nakama token 冒充 `iss=gamebackend` | 401（密钥/签名不符） |
| 未知 `iss` | 401 |

## 6. 验证

| 层 | 用例 |
|---|---|
| e2e | 匿名登录 401；玩家 token 登录 401；服务凭证登录 200；缺 `external_id` 400 |
| 平台（JWT 模式） | 匿名登录 401 → 服务登录拿到 **JWT**（`eyJ` 开头）→ 用该 token 读自己档案 200；错密钥/过期 token 401 |
| `pkg/httpx` | 后端 token 被接受且**不触发 resolver**；错密钥签发被拒；未知 iss 被拒；过期（严格 leeway）被拒；签发器缺 key 被拒、空 player id 被拒 |
| 契约 | `apigen -check` 通过；`account.login` 的 `security` 已变为 `serviceToken` |
| 真栈 | CI 的 `nakama` job 仍全绿（Lua 模块用服务凭证调登录，`clients/minimal` 用 RPC 拿 token） |

## 7. 回滚

回滚 = 把 `account.login` 的 `Auth` 改回 `AuthPublic` 并清空签发器。**不建议**：
那会重新打开冒充洞。数据无变更。

## 8. 未决

- [ ] token 吊销（例如封号立即生效）
- [ ] 是否为玩家 token 增加 `aud`（区分平台/客户端类型）
- [ ] `clients/minimal` 尚未覆盖"token 过期 → 重放 RPC"这条路径的自动化断言