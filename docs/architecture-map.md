# 当前架构与未来功能（带标注）

> **可视化版本**：`docs/architecture-map.html`（浏览器直接打开，内联 CSS/SVG，可离线/打印）。
>
> 这份文档把「现在长什么样」和「还缺什么」画在一起，每个方框、每一行都带状态标注。
> 代码是唯一事实来源；本文与代码不一致时以代码为准，并请顺手修正本文。
>
> 最后更新：2026-11-03（盘点修正：告警 7 条、CI 版本门禁、§7 陈旧行合并）

## 图例

| 标记 | 含义 |
|---|---|
| ✅ | **已验证**：CI 真跑或真机实跑过，可复现 |
| 🟡 | **已实现，未验证**：代码在、测试在，但缺少真实环境或尚未接线 |
| ⬜ | **未实现**：有设计/契约，没有代码 |
| 🔜 | **计划中**：只在路线图里 |

---

## 1. 部署拓扑（当前）

```mermaid
graph TB
  subgraph CLIENT["客户端侧"]
    UE["UE5 客户端<br/>⬜ 未实现<br/>只有 Go 参考模型 clients/minimal ✅"]
    MIN["clients/minimal<br/>✅ 极简客户端模型"]
  end

  subgraph HOST["单机主机（生产目标）"]
    NK["Nakama + Go 插件<br/>✅ 真容器登录链路 · ADR 0011"]
    BE["backend：platform 单进程<br/>✅ 7 域 · 22 op · spec 0001/0002"]
    PG[("PostgreSQL 16<br/>✅ 4 迁移 · 48 条断言 · spec 0006")]
    SM["Prometheus<br/>🟡 规则文件已写，未接入真实实例"]
  end

  subgraph VENDOR["托管战斗服：Edgegap"]
    DS["参考 DS 镜像 hape-ds<br/>✅ 容器内真打一局 · spec 0012"]
    UEDS["UE5 Dedicated Server<br/>⬜ 未构建（契约已定）"]
  end

  UE -.->|"① 设备登录"| NK
  MIN -->|"① 设备登录"| NK
  NK -->|"② RPC login：服务凭证换玩家 token"| BE
  UE -->|"③ 业务 HTTP（后端签发的 token）"| BE
  MIN -->|"③ 业务 HTTP"| BE
  BE -->|"④ 分配 / 释放（带状态回读）"| DS
  DS -->|"⑤ ready + 权威战报（双凭证）"| BE
  UE -.->|"⑥ 直连 UDP：address:port 来自 by-match"| DS
  BE --> PG
  BE -.->|"/metrics"| SM
```

**读图要点（权威边界，rules/90）**

| 决策 | 权威 | 客户端能否影响 |
|---|---|---|
| 局内命中/伤害/胜负 | DS | ❌ 只送输入 |
| 实例是否存在/地址端口 | Allocator（Edgegap） | ❌ |
| 谁和谁匹配 | matchmaking 服务 | 只能决定自己排不排 |
| 结果如何进入经济/进度/排行 | session 服务 + platform sink | ❌ |
| 玩家身份 | Nakama | 提供凭据，不决定身份 |
| 余额/价格/掉落 | economy 服务 | ❌ |

---

## 2. 一局对局的时序（标注了验证点与 spec）

```mermaid
sequenceDiagram
  autonumber
  participant C as 客户端
  participant N as Nakama
  participant B as backend
  participant V as Edgegap
  participant D as DS（参考镜像 / 未来 UE5）

  C->>N: 设备登录 ✅ spec 0009
  N->>B: RPC login → account.login（服务凭证）✅
  B-->>C: player_id + 后端 token ✅ spec 0011
  C->>B: 入队 ticket ✅
  Note over B: worker pop → session.create（含 players）✅ spec 0003/0013
  B->>V: 部署（env: SESSION_ID/RUNTIME_TOKEN/PLAYERS）✅ 真机 4s 就绪
  V->>D: 启动容器
  D->>B: mark_ready（服务凭证 + X-Session-Token）✅ spec 0008
  C->>B: 票据 → match_id → by-match 找地址 ✅ spec 0013
  C->>D: UDP 握手 5/5 回声 ✅ spec 0013
  D->>B: submit_result（同一 result_id，幂等）✅
  Note over B: 单事务扇出：战报存档 + 进度 + 经济 + 排行 + 会话完成 ✅ spec 0005
  B->>V: Release = 停止 + 回读 running=false ✅ 真机 ~2s
  C->>B: 轮询到 finished，读自己的收益 ✅
```

---

## 3. 代码结构与依赖

```mermaid
graph LR
  subgraph ENTRY["进程入口"]
    P["cmd/platform<br/>✅"]
    R["cmd/rank<br/>🟡 拆分路径示范，未单独部署"]
  end

  subgraph PLAT["platform/：装配层"]
    ASM["services 装配 + 路由挂载<br/>✅"]
    SINK["sink.go：结算扇出<br/>✅ 单事务 spec 0005"]
    REC["RunReconciler：超时会话 + 回收重试 + 指标<br/>✅ spec 0015"]
  end

  subgraph SVC["services/：7 个领域包"]
    AC["account ✅"]
    TM["team ✅"]
    MM["matchmaking ✅"]
    SE["session ✅"]
    PR["progress ✅"]
    EC["economy ✅"]
    RK["rank ✅"]
  end

  subgraph PKG["pkg/：基础设施"]
    API["api：Op/Shape → OpenAPI ✅"]
    ERR["apierr：唯一错误模型 ✅"]
    HT["httpx：路由/鉴权/指标 ✅"]
    BA["battlealloc：local ✅ / edgegap ✅ / gamelift 🔜 / agones 🔜"]
    CFG["config ✅ / logx ✅ / pg ✅"]
  end

  subgraph STORE["存储：每个域两套实现"]
    MEM["内存 Store ✅"]
    PGS["Postgres Store ✅ 等价性测试"]
  end

  P --> PLAT
  R --> PLAT
  PLAT --> SVC
  SVC --> PKG
  SVC --> MEM
  SVC --> PGS
```

**契约链路（防漂移）**：`api.Op` 反射 → `tools/apigen` → `api/openapi/*.json` → CI 用 `apigen -check` 拦住未重新生成的改动。✅

---

## 4. 结算扇出（钱的那条路）

```mermaid
graph TB
  DSREP["DS 战报<br/>result_id 幂等 ✅"] --> RES["session.submit_result<br/>✅ 双凭证校验"]
  RES --> TX{{"一个数据库事务 ✅<br/>ADR 0007"}}
  TX --> MR["match_results + match_player_results"]
  TX --> PG2["progress：经验 + 幂等流水"]
  TX --> EC2["economy：钱包 + 账本流水"]
  TX --> RK2["rank：评分 + MMR 事件"]
  TX --> SS["sessions.state = finished"]
  RES --> REL["Release（best-effort，失败进 release_pending）✅"]
```

要点：扇出**全部成功才**把会话置 `finished`；失败返回 503，调用方用**同一个** `result_id` 重试，每个下游按幂等键去重。回收失败不影响战报被接受。

---

## 5. 数据模型（14 张表，✅ 全部有写入）

| 域 | 表 | 标注 |
|---|---|---|
| account | `players` | ✅ |
| team | `teams`、`team_members` | ✅ |
| matchmaking | `matchmaking_tickets` | ✅ |
| session | `sessions`、`session_players`、`match_results`、`match_player_results` | ✅ ＋ 0003 的 `release_pending`/`release_attempts` |
| progress | `player_progress`、`progress_grants` | ✅ |
| economy | `wallets`、`ledger_entries` | ✅ |
| rank | `player_ratings`、`rating_events` | ✅ |
| 迁移记账 | `schema_migrations` | ✅ 4 个迁移，含 checksum 防篡改 |

---

## 6. 安全与凭证（当前状态）

| 机制 | 状态 | 说明 |
|---|---|---|
| 玩家身份 | ✅ | Nakama JWT（`iss=nakama`）→ 解析成 player_id；后端签发 token（`iss=gamebackend`） |
| 登录收口 | ✅ | `account.login` 服务专属，客户端不能自报身份（ADR 0010） |
| DS 双凭证 | ✅ | 服务 token（进程）+ 每场 `X-Session-Token`（会话）；缺一个就 401（spec 0008） |
| 凭证轮换 | ✅ | `BACKEND_SERVICE_CREDENTIALS` 多凭证并行 + `caller=` 日志（spec 0014） |
| **最小权限（scope）** | ✅ | 凭证声明可调 service（`name=token:svc+svc`，`*` 仅运维）；Router 按 `op.Service` 判定，越权 **403**（不是 401）；默认拒绝；拼错 scope 启动失败；`BACKEND_SERVICE_SCOPE_ENFORCE=false` 观察模式；覆盖性测试盯住示例 env。spec 0016 |
| **op 级 scope** | ✅ | 凭证可写 `service.op`（如 `account.login`）；同域兄弟 op 403；拼错启动失败（spec 0020） |
| 入站限流 | ✅ | 按玩家/凭证的令牌桶，429 + `Retry-After`（spec 0020） |
| mTLS / 短期令牌 | 🔜 | M4 |

---

## 7. 可观测性

| 能力 | 状态 |
|---|---|
| JSON 日志（`request_id`/`op`/`status`/`caller`） | ✅ |
| `/metrics`：请求计数、启动时间 | ✅ |
| 业务指标：`backend_release_*`（5）、`backend_service_scope_*`（2）、`backend_sessions_*` / `backend_oldest_*`（6）、`backend_unreleased_*`（2） | ✅ spec 0015 / 0016 / 0018 / 0019 |
| 告警规则：8 条（回收卡住、未确认分配、分配卡住、僵尸对局、5xx 比例、结算延迟、越权调用、经济 500） | ✅ 规则 + promtool CI + 指标漂移测试（spec 0019） |
| 告警规则文件（8 条，`deploy/prometheus/hape.rules.yml`） | 🟡 规则与 promtool 校验在；**没有接真实 Prometheus 实例**，所以"人为触发一次"未做过 |
| 告警的静默/抑制与值班路由（Alertmanager） | 🔜 部署选择，不属代码 |

---

## 8. CI（8 个 job，✅ 全绿）

| job | 覆盖 | 标注 |
|---|---|---|
| `verify` | gofmt / vet / build / `go test -race`（含真库等价性）/ schema 探针 / 契约漂移 | ✅ |
| `image` | 构建后端镜像、非 root、探活、/metrics | ✅ |
| `compose` | postgres + backend 起栈、迁移、48 条断言、断言一次登录落库 | ✅ |
| `nakama-module` | Go 插件的快速类型检查 + 三方版本一致性（真构建在 `nakama` job 的镜像里，产物级断言也在那里） | ✅ |
| `nakama` | 登录链路 + **客户端打完整一局**（Nakama 身份 + 容器 DS） | ✅ |
| `ops` | 迁移幂等/append-only、备份恢复（恢复到新库）、pg 客户端版本护栏 | ✅ |
| `ds` | DS 镜像、缺 token 拒绝启动、容器/进程各打一局、崩溃重启一次结算、dev 身份完整对局 | ✅ |
| `release` | 后端镜像 + DS 镜像推送 ghcr 并回拉探活 | ✅ |

---

## 9. 未来功能 → 独立路线图

已拆成独立页面，避免同一张表在多个文档里漂移：

- `docs/roadmap.html`：可视化路线图（浏览器打开）
- `docs/roadmap.md`：Markdown 源

里面有：三条并行主线（观测→容量 / 引擎内 UE5 / 平台演进）、现在就能做且不被外部条件阻塞
的三项（含完成判据）、里程碑 M2–M5、13 项明确未实现、以及已关闭清单（对照成本与节奏）。

本页只讲**现状**：每个方框是什么、做什么、验证到哪一步。
## 10. 一句话总结

**后端闭环和战斗服闭环都已经真跑通并可复现（22 op / 7 域 / 14 表 / 8 个 CI job 全绿），
未验证的部分集中在"外部世界"：UE5 引擎里的两个二进制、真实 Prometheus、厂商账单、
以及一台真的生产机器。**
