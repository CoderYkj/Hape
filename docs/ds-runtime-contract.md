# DS 运行时契约 v1（评审稿）

- 状态：**草案 v1** → 待 UE5 引擎组评审 → 定稿
- 适用：任何给本后端当专用服务器的进程。参考实现是 `ds/`（Go，可跑、可演练）；
  未来的 UE5 DS 替换那个二进制，**变量名与两个接口不变**。
- 配套：`docs/ue5-ds-integration.md`（怎么实现）、`ds/README.md`（参考实现）、
  spec 0012（为什么这样定）。本文是**给引擎组评审/签署的那一份**。

## 0. 一句话

DS 只需做三件事：**报 ready**、**打一局**、**报一次战报**。后端回它四种结果。
其余（账号、匹配、经济、排行）都不在 DS 的职责内，DS 也不该知道。

## 1. 环境变量：分两层，别混

`per-match`（后端每次分配时注入，一场一变）与 `deployment`（应用版本的固定环境，
部署时配置一次）是两类东西。**只有 per-match 的那些会影响结算正确性。**

| 变量 | 层级 | 必填 | 说明 |
|---|---|---|---|
| `BATTLE_SESSION_ID` | per-match | ✅ | 这一场所属会话，上报 URL 里要用 |
| `RUNTIME_TOKEN` | per-match | ✅ | 这一场的凭证，`X-Session-Token` 用；**只进内存，不进日志** |
| `PLAYERS` | per-match | ✅ v0 | JSON 数组，后端 `player_id`（不是 Nakama user id）。v0 战报必须与它一致 |
| `MATCH_ID` / `MAP` | per-match | — | 透传进日志与战报头 |
| `DS_PORT` | per-match | — | UDP 端口，默认 7777 |
| `BACKEND_BASE_URL` | deployment | ✅ | 后端基址（内网地址即可） |
| `BACKEND_SERVICE_TOKEN` | deployment | ✅ | 进程身份；凭证的 scope **必须包含 `session`** |
| `DS_ADVERTISE_ADDRESS` | per-match | — | 玩家实际拨的地址；**不知道就省略**（见 §2.1） |
| `RESULTS_DIR` | per-match | — | `result_id` 落盘目录，默认 `Saved/Results` |

> Edgegap 侧由适配器注入 per-match 的六项（`BATTLE_SESSION_ID/MATCH_ID/DS_PORT/PLAYERS/MAP/
> QUEUE/RUNTIME_TOKEN`），deployment 两项由 app version 的环境配置提供。
> **不要**把 `BACKEND_SERVICE_TOKEN` 放进 per-match 注入——它一场一变没有意义，还会泄漏面变大。

## 2. 三个义务

### 2.1 上报 ready（`Listen` 成功之后）

```text
POST {BACKEND_BASE_URL}/v1/session/sessions/{BATTLE_SESSION_ID}/ready
Authorization: Bearer {BACKEND_SERVICE_TOKEN}
X-Session-Token: {RUNTIME_TOKEN}
Content-Type: application/json

{"address": "1.2.3.4", "port": 7777}      // 两个字段都可省略
```

- **address/port 可省略**：容器内通常无法知道自己的公网地址；省略时后端保留厂商分配的地址。
  只有在自建 fleet、DS 自己知道公网地址时才自报（自报值优先）。
- 重复 ready 是**幂等**的（200），可以安全重试。
- 重试间隔建议 2s → 4s → 8s，最多 5 次；仍失败**不要让玩家进**（否则这一局无法结算）。

### 2.2 局内权威

命中/伤害/死亡/胜负只由 DS 决定；后端不参与、客户端只送输入。DS 不需要、也不应该向后端
上报过程数据（v1 不做逐帧或中途状态同步）。

### 2.3 上报战报（对局结束时，**且只报一次**）

```text
POST {BACKEND_BASE_URL}/v1/session/sessions/{BATTLE_SESSION_ID}/results
Authorization: Bearer {BACKEND_SERVICE_TOKEN}
X-Session-Token: {RUNTIME_TOKEN}

{
  "result_id": "<本局结果标识，见 §3>",
  "duration_seconds": 180,
  "players": [
    {"player_id": "p_xxx", "won": true,  "score": 750, "kills": 7, "deaths": 3, "damage": 1400},
    {"player_id": "p_yyy", "won": false, "score": 420, "kills": 3, "deaths": 7, "damage": 800}
  ]
}
```

硬性要求：

1. `players` 必须与后端 session 的名单**完全一致**（多一个、少一个、重复 → 400）；
2. `player_id` 用后端给的 id（`PLAYERS` 里的、或 join 事件里的），**不是** Nakama user id，
   也不是客户端传入的任何 id；
3. `duration_seconds` 非负整数；
4. `result_id` 见 §3——这一条是整套契约里最容易做错、后果最贵的一条。

## 3. `result_id`：一局只能结算一次

规则：

1. 一局开始时生成一次，格式建议 `<session_id>-<开始时间戳 utc>-<4 位随机>`；
2. **先落盘再上报**（`{RESULTS_DIR}/<session_id>.json`），崩溃重启后读回同一个值；
3. 上报成功后标记完成（同一个文件里写 `reported_at`），重启的进程看到标记就退出；
4. **绝不能**每次上报时现生成——那会把一次对局变成两次结算。

参考实现的做法与演练见 `ds/README.md`；`tools/fakeds -crash-restart` 就是验这条的。

## 4. 响应语义：DS 看到什么，就该做什么

| 响应 | 含义 | DS 行为 |
|---|---|---|
| 200 `applied=true` | 首次结算成功 | 标记完成，停止重试 |
| 200 `applied=false` | 同一 `result_id` 重放 | 标记完成（**不是错误**） |
| 400 `invalid_argument` | 字段不合法（players 与 session 不一致等） | **不重试**，告警并保留日志（DS 的 bug） |
| 401 `unauthenticated` | 凭证缺失/错误 | **不重试**，告警 |
| 403 `permission_denied` | 凭证是真的，但 scope 不含 `session` | **不重试**，找后端加 scope |
| 409 `conflict` | 该 session 已有**另一份**结果 | **不重试**，告警（DS 逻辑有问题） |
| 412 `failed_precondition` | 状态不允许（会话还没 ready / 已 failed） | 短暂退避后重试；先 `GET` 确认状态 |
| 503 `unavailable` | 下游扇出失败 | **用同一个 `result_id`** 退避重试（幂等安全） |

## 5. 后端如何验收一台 DS（可执行清单）

引擎组交付前，后端会跑这 6 条；建议引擎组自己先跑一遍：

| # | 场景 | 期望 | 怎么验 |
|---|---|---|---|
| 1 | 容器里不注入 `RUNTIME_TOKEN` | **拒绝启动**，并点名缺哪个变量 | 参考实现：`docker run` 少传一个 env，退出码非 0 |
| 2 | 战报少了/多了玩家 | 400，且 DS 不重试 | 手工构造 body |
| 3 | 同一 `result_id` 连报两次 | 第一次 200 applied=true，第二次 200 applied=false | 手工重放 |
| 4 | 换一个 `result_id` 再报 | 409，DS 停止 | 手工构造 |
| 5 | 上报前杀进程再重启 | 复用同一个 `result_id`，全局只结算一次 | `fakeds -crash-restart` 的做法 |
| 6 | 重复 ready | 200 幂等 | 手工连发两次 |

## 6. 待评审拍板的 6 件事（附我的建议）

| # | 问题 | 我的建议 | 如果不这样 |
|---|---|---|---|
| 1 | **玩家进入事件**：`ready → in_progress` 谁置位？ | 新增一个服务 op `session.mark_in_progress`（双凭证，幂等），DS 在第一个玩家 join 后调用 | 超时清扫只能对 `ready` 一刀切，无法区分"没人进"与"正在打" |
| 2 | **名单来源**：env `PLAYERS` 还是 DS 从 join 自己攒？ | v1 **两者都支持**：`PLAYERS` 仅用于验收/演练；生产以 join 为准，战报校验改为"覆盖后端 session 名单" | 若只用 env，真实对局里加入/掉线的玩家无法表达 |
| 3 | **掉线重连** | v1 **不承诺**；DS 侧按"该玩家本局失败/放弃"处理，后端不做会话恢复 | 重连需要 DS 保存连接状态与后端保存连接态，属独立特性 |
| 4 | **公开地址** | DS **省略** address/port，用厂商分配的地址（容器内拿不到公网 IP） | 自报 `127.0.0.1` 会让客户端连不上 |
| 5 | **战报 schema 版本** | v1 冻结字段；UE5 上线后加 `schema_version`（向后兼容：无版本= v1） | 将来改字段时无法安全重放旧战报 |
| 6 | **日志纪律** | 必须打印 `session_id` / `result_id` / 上报结果；**禁止打印凭证**（只打长度） | 排障时无锚点，或凭证进日志变成泄漏面 |

## 7. 变更流程

- 本文是 **v1**。破坏性变更（变量名、必填性、字段语义）→ **v2**，并给一段双写期。
- 新增**可选**变量或字段不升版本，但要同时改：本文 + `ds/` + Edgegap 注入 + 验收清单。
- 任何一方想改契约，先改本文再改代码；代码先改而文档后补，等于没有契约。

## 8. 45 分钟评审议程

1. （10 分钟）走 §2/§3：三个义务 + result_id——这是不能有分歧的部分；
2. （20 分钟）逐条拍 §6 的 6 件事，每条记录"决定 + 谁负责"；
3. （10 分钟）确认 §5 验收清单：引擎组什么时候能跑通这 6 条；
4. （5 分钟）定变更流程与下一次评审时间。

## 9. 写这份契约时发现的集成缺口（已修，记录事实）

契约必须由代码验证，否则只是一段文字。这次核对时就抓到三个：

1. Edgegap 适配器注入的是 `SESSION_ID`，参考 DS 读的是 `BATTLE_SESSION_ID` → 真实部署会
   直接"缺变量拒绝启动"；
2. `PLAYERS` 根本没注入 → DS 无法构造战报（战报必须覆盖名单）；
3. `MAP` / `DS_PORT` 没注入 → 地图与端口只能靠默认值。

三处已改为按契约注入，并有适配器契约测试断言这些键存在（含 `PLAYERS` 的 JSON 形态、
`DS_PORT` 取配置端口）。这也是为什么 §5 是一份**可执行**清单而不是承诺。