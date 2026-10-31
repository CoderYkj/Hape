# 架构总览

> 面向新加入的工程师 / AI Agent。读完应能回答：谁说了算、数据在哪、一次对局经过哪些组件。

## 1. 三个平面

```mermaid
flowchart TB
    subgraph Client["客户端（UE5 游戏）"]
        C1[登录 / 队伍 / 匹配 UI]
        C2[战斗客户端]
    end

    subgraph Social["社交平面 · Nakama"]
        N1[多平台登录鉴权]
        N2[好友 / 公会 / 聊天]
        N3[Go 运行时插件<br/>只做转发与事件钩子]
    end

    subgraph Domain["领域平面 · Go 服务（本仓库）"]
        A[account]
        T[team]
        M[matchmaking]
        S[session]
        P[progress]
        E[economy]
        R[rank]
        BA[pkg/battlealloc<br/>分配抽象层]
    end

    subgraph Battle["战斗平面 · 托管专用服"]
        V[厂商调度<br/>GameLift / Edgegap / Agones]
        DS[UE5 Dedicated Server<br/>局内唯一权威]
    end

    C1 -->|device/Steam/Apple| N1
    N1 -->|外部身份| A
    C1 --> T
    C1 --> M
    C1 -->|轮询地址| S
    M -->|形成 match| S
    S --> BA
    BA --> V
    V -->|分配实例| DS
    DS -->|mark_ready| S
    DS -->|submit_result 权威战报| S
    S -->|扇出| P
    S -->|扇出| E
    S -->|扇出| R
    C2 -->|直连 address:port| DS
```

**权威一句话**：战斗中 DS 说了算，战斗外 Go 说了算，Nakama 只做社交与鉴权。
详见 `.harness/rules/90-battle-integration.md`。

## 2. 服务与数据归属

| 服务 | 拥有的数据 | 不会做的 | v0 端口（拆分时） |
|---|---|---|---|
| account | `Player`（后端 id ↔ Nakama id ↔ 显示名） | 校验密码（Nakama 做） | 8081 |
| team | `Team`、`Member` | 公会/聊天（Nakama 做） | 8082 |
| matchmaking | `Ticket`（含 MMR 快照） | 决定去哪台机器 | 8083 |
| session | `Session` 生命周期、战报去重 | 战斗判定 | 8084 |
| progress | `Progress`（XP/等级/解锁） | 发货币 | 8085 |
| economy | `Wallet` + 账本 | 决定掉落（DS 决定，econ 执行） | 8086 |
| rank | `Rating` / 排行 | 计算单局得分 | 8087 |
| platform | 无（装配 + 路由） | 业务规则（只有奖励映射） | 8080 |

v0 全部跑在一个进程内，端口只是拆分时的约定。

## 3. 一次对局的时间线

```mermaid
sequenceDiagram
    autonumber
    participant P as 玩家客户端
    participant DS as UE5 Dedicated Server
    participant N as Nakama
    participant A as account
    participant M as matchmaking
    participant S as session
    participant AL as Allocator/厂商
    participant ECO as economy / progress / rank

    P->>N: 登录（device/Steam…）
    N-->>P: Nakama session
    P->>A: POST /v1/account/sessions {external_id}
    A-->>P: player_id + session_token
    P->>M: POST /v1/matchmaking/tickets {queue}
    M-->>P: ticket(waiting)
    Note over M: matchmaker worker 定时 pop
    M->>M: pop → match_id，ticket=matched
    M->>S: POST /v1/session/sessions {match_id, players}
    S->>AL: Allocate(session)
    AL-->>S: address:port + allocation_id
    S-->>M: session(allocating)
    P->>S: GET /v1/session/sessions/{id}（轮询）
    S-->>P: {allocating} … {ready, address, port}
    DS->>S: POST …/{id}/ready {address, port}
    P->>DS: 连接并进入对局（客户端输入）
    Note over DS: 局内权威：命中/伤害/胜负
    DS->>S: POST …/{id}/results {result_id, players[]}
    S->>ECO: 扇出（幂等 by result_id）
    S-->>DS: 200 finished
    P->>S: GET 会话（finished）
    P->>ECO: GET wallet / progress / leaderboard
```

## 4. 关键设计取舍

| 取舍 | 选择 | 原因 | 记录在哪 |
|---|---|---|---|
| 社交自研 vs Nakama | Nakama | 小团队不重复造登录/公会/聊天 | ADR 0001 |
| 战斗权威 | UE5 DS | 只有它能跑同一套战斗代码 | ADR 0002 |
| 战斗服 | 托管 + 抽象层 | 无 K8s 运维；可换厂商 | ADR 0003 |
| 内部传输 | HTTP/JSON + 生成契约 | 工具链轻、curl 可调试 | ADR 0004 |
| 部署形态 | 模块化单体 | 迭代速度优先 | ADR 0005 |
| 存储 | 接口 + 内存实现 | 先证明链路，再加 DB | spec 0001 |

## 5. 容量与成本直觉（v0 量级）

- 单进程 Go 服务处理排队/会话/查询：千级 QPS 不是问题（瓶颈在 DS 与匹配等待）。
- 成本大头是 **DS 实例·秒**，不是后端。所以：
  - 会话结束必须尽快 `Release`；
  - 空转实例是纯亏损（见 90 文档"回收"一节）。
- Nakama 单实例（带 Postgres）足以支撑早期 DAU；先别做 Nakama 集群。

## 6. 演进路线（按顺序，别跳）

| 里程碑 | 内容 | 前置 |
|---|---|---|
| M0 ✅ | 垂直切片，内存存储，local allocator | — |
| M1 | Nakama 联调：真实登录 → `account.login` | Nakama 本地起得来 |
| M2 | Postgres + 迁移（`db/migrations`） | 确认 schema |
| M3 | 真实托管服适配器（一家）+ `Release` 接线 | 厂商账号与配额 |
| M4 | 对账任务（僵尸会话）、per-allocation token | M3 |
| M5 | 按热点拆服务（`cmd/<域>` 已有样例） | 单机压力见顶 |

## 7. 目录与依赖方向

```text
cmd → platform → services → pkg
```

- `pkg/*` 不得 import `services/*`。
- `services/a` 不得 import `services/b`；跨域只在 `platform` 接线。
- 新代码放对层比写得快更重要：放错的层会在拆分时爆炸。
