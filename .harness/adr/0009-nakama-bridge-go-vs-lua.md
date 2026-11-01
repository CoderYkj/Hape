# ADR 0009 - Nakama 运行时的桥接方式：Go 插件 vs Lua 模块

- 状态：**proposed**（数据收集中：`nakama` CI job 的 buildinfo 诊断）
- 日期：2026-01-01
- 关联：ADR 0001（Nakama 负责身份）、spec 0009（Nakama 联调）

## 背景

Nakama 的桥接 RPC（`login`：把 Nakama session 换成后端 player + token）可以用两种方式实现：

1. **Go 插件**（`.so`，`-buildmode=plugin`）。当前仓库就是这条路。
2. **Lua 模块**（`.lua`，放进 `data/modules`，Nakama 内置运行时，无需编译）。

## Go 插件的真实代价（本次联调实测）

`plugin.Open` 要求插件与宿主**共享的每一个包**的导出数据哈希一致。哈希取决于：

| 因素 | 本次踩到的实例 |
|---|---|
| Go 工具链版本 | 宿主 `runtime=go1.22.4`，插件用 1.21.x 构建 → `internal/goarch` 不一致 |
| 桥接库版本 | 宿主 `nakama-common v1.32.0`，插件 v1.35.0 → 不匹配 |
| 传递依赖版本 | 宿主 `protobuf v1.34.1`，插件 v1.35.2 → 不匹配 |
| 构建设置 | `CGO_ENABLED`、`GOAMD64`、build tags、`-trimpath` 同样参与哈希 |

失败方式是**启动即崩**（Nakama `fatal: Failed initializing runtime modules`），
而且错误只给一个包名，不给原因。这四次迭代的成本是：先猜版本、再猜工具链、
再猜依赖、最后才想到构建设置。

## 选项

| | Go 插件 | Lua 模块 |
|---|---|---|
| ABI 耦合 | **强**：工具链 + 全部共享依赖 + 构建设置 | 无（解释执行） |
| 需要编译 | 是（必须与镜像精确匹配） | 否 |
| 类型检查 | 有（编译期） | 无 |
| 代码复用（与本仓库其余 Go 代码） | 不能真正复用（插件是独立二进制） | 不能 |
| 桥接逻辑的复杂度 | 简单（HTTP + JSON） | 简单（`nk.http_request` + JSON） |
| 升级 Nakama 镜像 | 必须重建插件并重新对齐 | 通常无感 |
| 调试 | 插件内 panic 会拖垮宿主 | 运行时错误被隔离 |

## 关键判断

这层桥接的逻辑量约 30 行（读 user id、POST 后端、回传 JSON），
**没有任何需要 Go 类型系统或性能的地方**。为它承担"工具链/依赖/构建设置三方对齐"
的持续成本，收益不成立。

但如果 Go 插件能一次性对齐并且后续不再漂移（例如 CI 从镜像探测工具链 + 锁死依赖版本），
它的可维护性并不差——本次 CI 已经在做这件事。

## 待定（需要 buildinfo 数据）

- 若失败仅源于 `CGO_ENABLED`/`GOAMD64` 等**一次性设置**：修好并保留 Go 插件，
  同时把对齐规则写成 CI 断言（已部分实现）。
- 若无法稳定对齐：**改用 Lua 模块**，Go 插件降级为"可选/实验"，
  并在 `docs/ue5-ds-integration.md` 与 compose 里切换默认路径。

## 后果（任一选择）

- `nakama` CI job 都必须继续存在：它是唯一能证明"插件/模块真的被 Nakama 加载并能
  回调后端"的地方。
- 桥接契约（RPC 名 `login`、返回 `{player, session_token}`）不因实现语言而变。