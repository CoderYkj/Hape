# ADR 0010 - 封闭登录接口，并让后端签发自己的玩家 token

- 状态：**accepted**（2026-01-01 拍板）
- 关联：ADR 0001（身份归属）、spec 0006（Nakama JWT 认证）、spec 0011（本次实现）
- 影响：`account.login` 的认证级别（契约变更）、`pkg/httpx` 新增签发器

## 背景：两个问题，一个位置

`account.login`（`POST /v1/account/sessions`）现在有两条独立缺陷：

1. **它是 public 的**（OpenAPI `security: none`），body 里带 `external_id` 就直接返回
   那个玩家的 token。客户端开始直连后端之后（`clients/minimal`），这意味着
   **任何人可以冒充任意 Nakama 用户**，然后读钱包、花钱、排队。
2. **它无条件返回 `dev-player-<id>`**（`services/account/service.go:114`）。
   生产模式（`BACKEND_DEV_AUTH=false`）下 JWT 认证器不认这个格式，于是
   **生产环境的客户端拿不到可用的后端 token**——登录链路在 prod 实际是断的。

两条都源于同一个判断：把"Nakama 插件创建/兑换玩家"（service 调 service）与
"客户端凭身份调后端"（player 调 backend）混在了一个公共接口里。

## 选项

| | A. 登录改服务专属 + 后端签发玩家 JWT | B. 新增 exchange op，登录保留 public | C. 客户端继续用 Nakama token 直连后端 |
|---|---|---|---|
| 冒充风险 | 消除（客户端无法调登录） | 消除（但登录仍公开，靠 exchange 才安全） | 消除 |
| 客户端能否在 prod 拿到 token | 能（服务端签发） | 能 | 能 |
| 契约变化 | 1 个 op 改认证级别 | 新增 1 个 op + 旧 op 降级 | 认证器要认 Nakama token 并自行映射 |
| 后端是否多一跳 | 否 | 是（客户端多调一次 exchange） | 否 |
| 与现有客户端模型一致 | 是（客户端只拿 RPC 返回的 token） | 是 | 否（客户端要自己带 Nakama token） |

**选 A**：

- 客户端本来就不该知道"用 external_id 换 token"这件事——那是身份提供方（Nakama）
  与后端之间的事。把它变成服务专属，等于把接口的语义收紧到它真实拥有的能力。
- 少一个接口：不需要 exchange，因为 RPC 已经把 token 带回来了（`clients/minimal`
  已经这么用）。
- 生产 token 由后端签发，`JWTAuth` 增加"自己签的 token"分支，不再依赖
  "客户端必须持有 Nakama token"。

## 决定

1. `account.login` 的 `Auth` 从 `AuthPublic` 改为 `AuthService`：只有持服务凭证的
   Nakama 插件能调用它创建/兑换玩家。
2. 后端新增 **玩家 token 签发器**（HS256）：`iss=gamebackend`、`sub=<后端 player id>`、
   `exp/iat`，TTL 默认 1 小时。密钥默认复用 `BACKEND_JWT_KEY`，可用
   `BACKEND_PLAYER_JWT_KEY` 单独配置。
3. `JWTAuth` 按 `iss` 分流：
   - `iss=nakama` → 校验后用 `IdentityResolver` 把 Nakama 用户映射成后端玩家（现状）；
   - `iss=gamebackend` → `sub` 就是后端玩家 id，**不查库**；
   - 其它 `iss` → 401。
   两个分支用各自的密钥验签（未配独立密钥时同一把），并保持 HS256-only、
   常量时间比较、`exp` 必填这些既有约束。
4. dev 模式（`BACKEND_DEV_AUTH=true`）保持返回 `dev-player-<id>`：本机测试工具、
   e2e、`fakeds` 都依赖它，且这个格式只在 dev 里被接受。

## 后果

- **契约变更**：`account.login` 的 security 从 none 变为 serviceToken。客户端不再调用它；
  `clients/minimal` 与 `docs/ue5-client-model.md` 的流程不变（RPC 拿 token）。
- e2e 与 `fakeds` 的登录调用要带服务凭证（它们本来就跑在可信的一侧）。
- 生产链路第一次真正闭环：Nakama 登录 → RPC（服务凭证）→ 后端签发 JWT →
  客户端用该 JWT 调后端。
- `README` 的"已知缺口"里那条**关闭**（不再是延后项）。

## 未决

- [ ] 玩家 token 的轮换/吊销：现在靠短 TTL + 客户端重放 RPC，没有黑名单
- [ ] 是否给玩家 token 加 `aud`（区分 web/mobile 客户端）