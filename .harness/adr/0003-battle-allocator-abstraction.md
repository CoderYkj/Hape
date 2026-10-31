# ADR 0003 - 战斗服分配走抽象层，早期用托管厂商

- 状态：accepted
- 日期：2026-01-01
- 决策者：服务端负责人

## 背景

"早期用托管战斗服"是既定前提：小团队没有也不该有 Kubernetes 集群运维能力。
候选：AWS GameLift、Edgegap（Nakama 有官方集成）、Agones（自建 K8s）、PlayFab。

问题是：如果直接按某家厂商的 SDK 写 `session` 服务，换厂商 = 重写会话层。
而早期最可能发生的事就是**换厂商**（价格、区域覆盖、配额、条款）。

## 决策

`session` 服务只依赖一个接口，不 import 任何厂商 SDK：

```go
type Allocator interface {
    Provider() string
    Allocate(ctx context.Context, req Request) (Allocation, error)
    Release(ctx context.Context, allocationID string) error
}
```

- 实现放 `pkg/battlealloc`，用 `BACKEND_ALLOCATOR` 选择。
- v0 只有 `local`（离线可用，给 `fakeds` 用）与三个 `Unconfigured` 占位。
- 占位实现返回 `ErrNotConfigured`：**明确失败，不静默降级到 local**。

## 备选方案与放弃原因

| 方案 | 优点 | 放弃原因 |
|---|---|---|
| 直接写 GameLift SDK | 最快接入一家 | 换厂商 = 重写会话层；本地无法离线跑 |
| 先自建 Agones | 完全可控、成本线性 | 需要 K8s + 节点池运维；小团队早期负担 |
| 用 vendor 的 matchmaking 一起做 | 少写代码 | 匹配规则与队伍/MMR 逻辑被锁进厂商；跨游戏复用难 |

## 后果

正面：

- 本地、测试、CI 全程跑 `local`，**零外部依赖**就能验证整条链路。
- 换厂商只写一个 adapter 文件，`services/session` 一行不改。
- 三家厂商的接入要点已记录在 `90-battle-integration.md`，接的时候不用重新调研。

负面：

- **抽象会漏**：托管厂商的独有概念（GameLift 的 fleet/game session、Edgegap 的 deployment request、Agones 的 CRD）无法完全塞进 `Request/Allocation`，接的时候大概率要给接口加字段。
- **`Release` 尚未接线**：v0 分配后不释放（本地无成本），接真实厂商时必须在结束/超时时调用，否则按秒计费空转。
- `Unconfigured` 的报错会在 staging 才暴露（这是设计意图：早失败），但意味着配置错误不会被本地测试发现。

## 触发重新评估的条件

- 接入第二家厂商时：如果接口需要大改，说明抽象方向错了，回来重写这一篇。
- 单日对局数超过托管厂商经济区间（自建 Agones 开始划算）。
- 出现"必须在指定区域/指定硬件规格"的需求，托管无法满足。
