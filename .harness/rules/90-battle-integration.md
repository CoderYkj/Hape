# 90 - 战斗集成（权威边界 / 分配 / 上报）

这是本仓库最容易出错的一篇规则。改动 session、matchmaking、battlealloc、或任何"谁说了算"的逻辑前必须读完。

## 1. 权威边界表

| 决策 | 权威 | 在哪个组件 | 客户端能否影响 |
|---|---|---|---|
| 局内命中、伤害、死亡、胜负 | **UE5 Dedicated Server** | 托管战斗服 | 否（只送输入） |
| 比赛实例是否存在、地址、端口 | **Allocator**（托管厂商） | `pkg/battlealloc` | 否 |
| 谁和谁匹配 | **matchmaking 服务** | `services/matchmaking` | 只能决定自己排不排 |
| 比赛结果如何进入经济/进度/排行 | **session 服务** | `services/session` + `platform/sink.go` | 否 |
| 玩家身份从哪来 | **Nakama** | `nakama/modules` → `account.login` | 提供凭据，不决定身份 |
| 队伍/成员 | **team 服务**（社交类留在 Nakama） | `services/team` | 可以邀请，不能改人数上限 |
| 余额、价格、掉落 | **economy 服务** | `services/economy` | 否 |

推论（都已在代码里强制）：

- 客户端调 `session.submit_result` → 401（e2e `client forging a battle report` 断言）。
- 上报里的 `players` 必须与 session 的 `players` 完全一致，多一个少一个都 400。
- 同一个 session 收到第二个不同的 `result_id` → 409，不覆盖。
- `mmr` 由服务端在入队时快照（`RatingLookup`），请求体里没有这个字段。

## 2. 会话状态机

```text
                    Allocate 成功
   [create] ─────────────────▶ allocating ──mark_ready──▶ ready
                                   │                        │
                                   │ Allocate 失败           │ 首个玩家进入
                                   ▼                        ▼
                                 failed                in_progress
                                                            │
                                          submit_result 成功 │
                                                            ▼
                                                        finished
```

合法迁移（`session.CanTransition` 是唯一实现，别处不得自造）：

| from | to | 触发 |
|---|---|---|
| allocating | ready | DS 上报监听地址 |
| allocating | failed | Allocator 失败 |
| ready | in_progress | 玩家进入（v0 可由 DS 或计时器置位） |
| ready | finished | 短局，没进入就结算 |
| in_progress | finished | 接收战报 |
| 任意 | failed | 托管实例丢失 |

**任意非法跳转返回 412 `failed_precondition`**，不是 500。

## 3. 三条调用链

### 3.1 分配

```text
matchmaker worker ──POST /v1/session/sessions──▶ session.create
                                                    │
                                                    ├─ 校验 match_id / players
                                                    ├─ 落库 state=allocating
                                                    └─ Allocator.Allocate(ctx, Request)
                                                          │
              ┌───────────────────────────────────────────┴──────────────┐
              ▼ 成功                                                     ▼ 失败
   state=allocating + address/port/provider                 state=failed + 503 unavailable
```

- 请求体字段（`CreateSessionRequest`）：`match_id`、`players`、`queue`、`region`、`map`。
- 分配失败必须把 session 置 `failed` 再返回 503，否则会出现永远 `allocating` 的僵尸会话。
- `session.create` **不是幂等**：重复调用会分配第二台机器。调用方超时后应 `GET /v1/session/sessions/{id}` 读回，而不是重发。

### 3.2 DS 就绪

```text
Dedicated Server ──POST /v1/session/sessions/{id}/ready──▶ session.mark_ready
                      body: {address?, port?}
```

- 同一 session 重复 ready 是幂等的（直接返回当前状态）。
- DS 上报的 address/port 优先于 Allocator 返回值（Allocator 只知道给了谁，DS 才知道自己监听在哪）。
- 生产版应在 ready 时校验 per-allocation token（v0 只校验 service token，见"待定项"）。

### 3.3 结算

```text
Dedicated Server ──POST /v1/session/sessions/{id}/results──▶ session.submit_result
        body: {result_id, duration_seconds, players[]}
                                   │
                                   ├─ result_id 与已记录相同 → 200 applied=false（幂等）
                                   ├─ 已有不同 result_id       → 409 conflict
                                   ├─ 状态不允许 finished      → 412
                                   ├─ validateResults 失败     → 400
                                   ├─ sink.MatchFinished()     ──┬─▶ progress.Grant
                                   │                             ├─▶ economy.Grant
                                   │                             └─▶ rank.Apply
                                   └─ 全部成功 → state=finished
```

**顺序很重要**：先跑扇出，再落 `finished`。扇出失败的语义是"战报收到了、下游没写成功"，返回 503，调用方用同一个 `result_id` 重试；每个下游都按 `result_id` 幂等，所以重放安全。

### 3.4 客户端拿地址

```text
client ──GET /v1/session/sessions/{id}──▶ {state, address, port, provider}
```

- 只能读自己在的 session（`players` 不含 caller → 403）。
- 客户端直连 DS，v0 不经后端 relay。
- 连接 token（`Allocation.Token`）目前只是占位；生产必须由 DS 校验。

## 4. Allocator 适配层

```go
type Allocator interface {
    Provider() string
    Allocate(ctx context.Context, req Request) (Allocation, error)
    Release(ctx context.Context, allocationID string) error
}
```

- `Request`：`SessionID/MatchID/Queue/Region/Map/MaxPlayers/Meta`。
- `Allocation`：`ID/Provider/Address/Port/Token/ExpiresAt`。
- `pkg/battlealloc.New(provider)` 解析 `BACKEND_ALLOCATOR`：

| 值 | 行为 |
|---|---|
| `local`（默认） | 返回 `127.0.0.1:7777+n`，离线可跑，给 `fakeds` 用 |
| `gamelift` / `edgegap` / `agones` | `Unconfigured` → 返回 `ErrNotConfigured`（明确失败，不假装成功） |

实现新适配器时必须满足：

1. `Allocate` 带超时（建议 10s）与可重试的错误分类（可重试：限流/5xx；不可重试：配置错误）。
2. 拿不到机器时返回错误，不要返回零值 `Allocation`。
3. `Release` 在"对局结束"与"清理超时会话"两条路径都要调到（已接：`session.submitResult` 成功结算后、`session.ExpireStale` 里各一次）。
4. **回收 = 停止请求 + 回读确认**：厂商的 200 只表示"接受了请求"。`Release` 必须轮询
   `/v1/status/{id}` 直到 `running == false`（或 404），超预算按可重试返回（spec 0015）。
   确认失败会写成 `sessions.release_pending`，由 reconciler 每轮重试；`release_attempts`
   持续增长就是漏计费的告警信号。回读失败**绝不能**让玩家的结算失败。
4. 不把厂商 SDK 类型泄漏到 `services/*`；适配器内部爱怎么用怎么用。

各厂商落点（供实现时参考，均未接入）：

- **GameLift**：`CreateGameSession` / `DescribeGameSessions` 拿 `IpAddress:Port`，`TerminateGameSession` 回收；需要 fleet + runtime 配置。
- **Edgegap**：`POST /v1/deployments` 拿 deployment request，轮询 status；Nakama 官方有 Fleet Manager 集成文档可对照。
- **Agones**：`GameServerAllocation` CRD 一次分配，`Address/Port` 直接给；回收靠 GameServer 生命周期回收。

## 5. 托管战斗服的现实约束（小团队）

- **早期不要自建 fleet**：用托管，把 `Allocator` 当成唯一接触面。
- **不要假设秒级分配**：托管常在 20–90s 量级。`allocating` 状态可以持续很久，客户端需要"准备中"UI。
- **分配会失败**：区域容量不足、配额、镜像拉取都会失败。失败必须落到 `failed`，并允许 matchmaker 重试新 session。
- **DS 会挂**：挂了就没有战报。需要一个对账任务把超时仍停在 `allocating`/`ready` 的 session 标记 `failed`。
  v0 已实现单机版：`platform.RunReconciler` 定时调 `session.ExpireStale`，
  由 `BACKEND_RECONCILE_INTERVAL` 开启（默认关），`BACKEND_SESSION_STALE_AFTER` 定阈值（默认 5m）。
  `in_progress` 刻意不扫——没有单局时长上限时，杀一个正在打的对局比漏一个更糟。
- **回收**：托管厂商按秒计费，空转的 DS 就是纯亏损。`Release` 必须在"对局结束 + TTL 到期"两条路径都调到，且必须回读确认（见 §4 第 4 条）。
- **扇出必须原子**：数据库模式下，战报存档 + 进度 + 经济 + 排行 + 会话完成状态在
  同一个事务里提交（ADR 0007）。不要在这个事务里加"顺手调用外部服务"的代码——
  HTTP 或厂商 API 会把这个跨域事务拖成一台慢查询。

## 6. 反作弊（v0 做到哪一步）

已做：

- 客户端无法提交战报（认证级别挡住）。
- 战报参与者必须与 session 完全一致（不能塞小号）。
- 同一局只接受一份结果（不能重放刷分）。
- 余额/进度/评分全部服务端计算，客户端只读。

未做（明确列出，避免误以为已完成）：

- 逐帧输入校验、位置合理性、射击命中回溯校验（属于 UE5 DS 侧）。
- 战报异常检测（击杀/时长/伤害分布离群）。
- 设备指纹与多开检测。

## 7. 本地联调

```bash
# 终端 1：起后端（local allocator）
go run ./cmd/platform

# 终端 2：模拟一台 Dedicated Server 跑完整一局
go run ./tools/fakeds

# 验证"分配失败"路径：换成未接线的厂商
BACKEND_ALLOCATOR=gamelift go run ./cmd/platform
# 然后 session.create 会返回 503 unavailable，session 状态为 failed
```

## 8. 待定项（诚实清单）

| 项 | 现状 | 需要做什么 |
|---|---|---|
| DS ↔ 后端认证 | 双凭证：service token（进程）+ 每场 `X-Session-Token`（会话） | 凭证轮换 / mTLS；真机确认 Edgegap 注入生效 |
| 会话回收 | 已接：结束 + 超时两条路径；回收带状态回读，未确认的进 `release_pending` 并由 reconciler 重试（spec 0015） | 真实厂商的时序复验；`release_attempts` 增长接告警 |
| 僵尸会话对账 | 已接：平台内定时器，opt-in | 多实例部署时的单活选主；`in_progress` 的时长上限 |
| 玩家进入事件 | ready 直接可结算 | DS 上报 `in_progress` |
| 掉线重连 | 未设计 | 需要 session 保存玩家连接状态 |
| 战报 schema 版本 | 无版本号 | 加 `schema_version` 字段 |
