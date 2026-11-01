# 0006 - 生产认证器：Nakama JWT

- 状态：implemented
- 日期：2026-01-01
- 关联 ADR：0001（Nakama 负责身份）、0006（生产 Authenticator 曾被列为债务）
- 涉及：`pkg/httpx`（新增 `jwtauth.go`）、`services/account`（新增只读查询）、
  `pkg/config`、`platform`、`cmd/platform`

## 1. 背景

`BACKEND_DEV_AUTH=false` 之前只有一种结局：平台**拒绝启动**，因为除了 `DevAuth`
没有任何认证实现。ADR 0006 把它列为"生产认证器缺失"，路线图挂在 M3。

拒绝启动是对的（宁可起不来，也不要拿 dev bearer token 上生产），但它意味着
`staging`/`prod` 完全没有可用的认证路径。这一篇补上路径本身。

## 2. 目标 / 非目标

目标：

1. 校验 Nakama 签发的 session token（HS256），从 claim 取用户身份。
2. 把该身份解析成**后端** player id，HTTP 层与 handler 仍然只看到 `Principal`。
3. 严格性：只接受 HS256、签名为常量时间比较、`exp` 必填、可选 `iss`/`aud`、
   容忍时钟偏差。
4. 服务间调用继续用共享 token（`BACKEND_SERVICE_TOKEN`），JWT 不能当服务凭证。
5. 缺配置启动即失败，不运行期逐个请求报错。

非目标：

- 不引入第三方 JWT 库（标准库 `crypto/hmac` + `encoding/json` 足够）。
- 不改 `account.login` 的认证级别（见第 6 节"已知缺口"）。
- 不做刷新令牌、吊销列表、多签发方联邦。

## 3. 设计

```text
Authorization: Bearer <Nakama JWT>
   → JWTAuth.Player
       ├─ header.alg 必须是 HS256（否则 401；挡住 alg:none / 算法混淆）
       ├─ HMAC-SHA256 签名，hmac.Equal 常量时间比较
       ├─ exp 必填；iat 不得在未来；可选 iss/aud；leeway 默认 60s
       ├─ 取 uid（回退 sub）作为外部身份
       └─ IdentityResolver(externalID) → 后端 player id
                                          ↓
                                   Principal{PlayerID, ExternalID}
```

要点：

- **`alg` 来自 token 自身**，所以必须钉死。接受 `none` 或让 token 挑算法是经典的
  JWT 绕过。
- **认证是只读的**。解析身份用 `account.LookupByExternalID`（新增的只读方法），
  不是 `FindOrCreate`：认证过程不该创建账号，更不该在鉴权路径上写库。
- **服务凭证与玩家凭证互不通用**：`ServiceToken` 只认共享 token，JWT 只认玩家。
- **`pkg/httpx` 不认识"玩家"这个域概念**，只认识"外部身份 → 后端 id"的解析函数，
  所以它仍然不 import `services/*`。

## 4. 配置

| 变量 | 何时必填 | 作用 |
|---|---|---|
| `BACKEND_DEV_AUTH=false` | — | 关闭 dev bearer token，启用 JWT |
| `BACKEND_JWT_KEY` | 关闭 dev auth 时必填 | Nakama 的 session 加密密钥 |
| `BACKEND_JWT_ISSUER` | 可选 | 校验 `iss`（Nakama 默认 `nakama`） |
| `BACKEND_SERVICE_TOKEN` | 关闭 dev auth 时必填 | 服务间调用凭证 |

`config.Validate` 与 `platform.New` 各拦一次：先查配置，再在构造 authenticator 时
检查 key/resolver 是否齐全。

## 5. 验证

| 层 | 用例 |
|---|---|
| 单元（`pkg/httpx`） | 有效 token；`sub` 回退；过期；无 `exp`；`iat` 在未来；错误密钥；篡改 payload；`alg:none`；两段式 token；非 base64；无 `uid`；缺 bearer；时钟偏差（1s vs 2min）；`iss`/`aud` 字符串与数组；解析器报错；服务 token 正误；JWT 与服务 token 不得互用；缺配置三种 |
| 装配（`platform`） | `DevAuth=false` + 正确 key：公共登录建号 → 带 JWT 读自己的档案 200；错误密钥 401；过期 401；无账号身份 401（且不建号）；`dev-player-*` 401 |
| 等价性（`services/account`） | 内存与 SQL 的 `LookupByExternalID` 行为一致：命中返回同一 player，未命中 `not_found` |
| 回归 | 无库全量、有库等价性、e2e、契约漂移 |

## 6. 已知缺口（本篇不解决，已写进 README）

`account.login`（`POST /v1/account/sessions`）仍是 **AuthPublic** 且信任 body 里的
`external_id`。也就是说：任何能访问后端的人都能声称自己是任意 Nakama 用户，拿到
那个玩家的后端 id 与 token。

**已关闭（ADR 0010 / spec 0011）**：登录改为服务专属（`AuthService`），客户端无法再
调用；生产模式的 token 由后端签发（`iss=gamebackend`），因此"客户端拿不到可用 token"
这个更严重的问题也一并解决。客户端现在只从 RPC 拿 token。

## 7. 回滚

纯新增 + 配置开关：去掉 `BACKEND_JWT_KEY` 并把 `BACKEND_DEV_AUTH` 设回 true 即回到
旧行为（dev）。没有数据变更。

## 8. 未决

- [x] `account.login` 的认证级别（见第 6 节）→ **已决定延后**（2026-01-01）：
  当前由 Nakama 插件用服务凭证调用；重开条件见 README「已知缺口」。
- [ ] Nakama 若改用 RS256（多实例/轮换密钥），需要加 JWKS 拉取与缓存
- [ ] 是否记录 `Principal.ExternalID` 到访问日志（现在日志只有请求行的 op/status）