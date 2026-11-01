# 0013 - 客户端对局闭环（匹配 → 找到服务器 → 连上 → 结算）

- 状态：implemented
- 日期：2026-11-01
- 关联 ADR：0002（DS 局内权威）、0004（HTTP/JSON v0）、0010（登录收口与玩家 token）
- 前置：spec 0009（Nakama 登录桥）、0012（DS 运行时契约）
- 涉及：`services/session`（新 op）、`clients/minimal`、`tools/fakeds`（worker 角色）、`scripts/ds-match-drill.sh`、CI

## 1. 背景

客户端那一半此前的终点是"入队 + 轮询票据"。`docs/ue5-ds-integration.md` §5 明说：

> 客户端拿不到 session id 时的兜底：`get_ticket` 返回 `match_id` 后，后端应能按 match
> 反查 session。这个读接口目前**没有实现**。

于是客户端链条断在这里：它知道自己在打哪一场（`match_id`），但**没有任何办法拿到要连的
地址**。这不是 UX 问题，而是"客户端根本无法进入对局"。

另外两个空白：没有任何东西验证过客户端**真的能连上**战斗服（`ds/` 的 `packets=0` 一直是
摆设），也没验证过匹配成功后客户端能不能读到自己的结算。

## 2. 目标 / 非目标

目标：

1. 一个客户端只凭票据上的 `match_id` 就能找到这场对局的服务器地址。
2. 客户端真的打开 socket、发 N 个包、收回 N 个包（双向证据，不靠日志推断）。
3. 结算由后端推给客户端可见：客户端等到 `finished`，再用自己的 token 读自己的收益。
4. 全链路在 CI 可重复：容器里的战斗服 + 独立进程的"厂商" + 独立进程的客户端。

非目标：

- 不做断线重连/会话恢复（需要 DS 侧保存连接状态，未设计）。
- 不做 UE5 转写编译（`docs/ue5-client-model.md` §6 是形状指引，未在引擎里验证）。
- 不改任何已有 op 的语义；不改认证级别。

## 3. 新 op：`session.get_by_match`

```text
GET /v1/session/sessions/by-match/{match_id}      Auth: player
  200 {session:{id,state,address,port,...}}      调用者在名单内
  403 permission_denied                          不在这一场的名单里
  404 not_found                                  这个 match 还没分配（客户端继续轮询）
  401 unauthenticated                            没有玩家凭证
```

为什么是**路径**而不是 `?match_id=`：`pkg/api.Op` 目前只建模路径参数（`Request` 是 body），
所有既有读接口也都是路径参数；查询参数会绕过生成器，契约文档里看不到。等真的需要分页
/过滤再给 `Op` 加 query 模型，那时是一次契约层改动，不是顺手加。

与其他读接口一致的约束：

- **player-scoped**：只有名单内的玩家能读（`playerView`），与 `session.get` 共用同一条
  出口，杜绝两条读路径行为漂移。
- **凭证清零**：`runtime_token` 在返回前清空，且字段是 `omitempty`——玩家侧的 JSON 里
  根本没有这个键。
- **失败关闭**：查不到就 404，而不是返回空壳 session 让客户端去猜。

## 4. 客户端九步（`clients/minimal`）

| 步 | 动作 | 断言 |
|---|---|---|
| 1 | 身份（Nakama device auth + RPC `login`；`IDENTITY=dev` 时直接用开发身份） | 拿到 `player_id` + 后端 token |
| 2 | 读自己的档案 | 200 |
| 3 | 入队 | 拿到 `ticket.id` |
| 4 | 轮询票据 | `waiting` 或 `matched` |
| 5 | 第二个身份入队（`CLIENT_BOT=1`） | 队列能凑出一场（生产里这是另一个真人） |
| 6 | 等匹配 | 票据出现 `match_id` |
| 7 | `by-match` 找服务器 | `state=ready` 且 `address:port` 非空 |
| 8 | 连 UDP：发 N 个包收 N 个回包 | `ECHOES=N` |
| 9 | 等 `finished`，读自己的收益 | `total_xp > 0` |

第 5–9 步需要另一侧有"厂商"，所以由 `CLIENT_FULL=1` 开启；默认只跑前四步（不需要额外
进程，CI 的 `nakama` job 既跑默认形态也跑完整形态）。

## 5. 为什么参考 DS 要对收到的包回声

参考实现不实现游戏协议，但"客户端真的连上了"必须**双向**证明：只看服务端日志
（`packets=N`）无法区分"客户端发了"和"服务端自说自话"。所以 `ds/` 把收到的包原样回给
来源地址，客户端据此断言 `ECHOES=N`。UE5 真机这里换成正常的复制与输入处理。

## 6. provider 侧不能自己造名单（`fakeds -mode worker`）

`play` 模式自己登录两个玩家，客户端永远只能旁观：session 的名单里没有它。`worker` 模式
先 `pop` 真实队列，用**票据里的名单**建 session，再启动战斗服——这才是客户端能被分到
服务器的那条路径。

worker 没有玩家凭证，因此它不读任何人的钱包（`session.get` 是 player-scoped，这是设计）：
它用战斗服的 `applied=true` 作为"会话已 finished 并提交"的确认，收益由客户端自己读。

## 7. 失败模式

| 场景 | 结果 |
|---|---|
| 没有 provider 在跑 | 客户端 60s 后报 `no match after ...`（提示去起 `-mode worker`） |
| 队列凑不满 2 人 | provider 90s 后报 `no match formed in 90s` |
| 分配失败 | `by-match` 返回 `state=failed`，客户端立即报错而不是等到超时 |
| 战斗服没监听 | `ECHOES` 不足，客户端在 5s 读超时后失败，并指出第几个包 |
| 结算没落地 | `total_xp=0` → 客户端失败（"没结算的胜利"是 bug，不是可接受状态） |

## 8. 验证

| 层 | 用例 |
|---|---|
| 服务单测 | `by-match` 命中/非名单 403/匿名 401/不存在 404，且玩家侧读不到 `runtime_token` |
| 存储等价性 | 内存与 SQL 的 `GetByMatch` 一致（命中已完成的会话、未知 match 404） |
| 契约 | `apigen -check` 无漂移（`session` 从 4 个 op 变 5 个） |
| 本机实跑 | 三个独立进程：客户端 `ECHOES=5`、`SESSION_STATE=finished`、`TOTAL_XP=125`、`CLIENT_OK=1`；worker 退出码 0；战斗服 `packets=5 echoed=5 applied=true` |
| CI `ds` job | `IDENTITY=dev` 的完整对局（容器战斗服） |
| CI `nakama` job | `IDENTITY=nakama` 的完整对局（真 Nakama + RPC 兑换）+ 默认四步协议检查 |

## 9. 未决

- [ ] 断线重连：需要 DS 侧保存每局的连接状态，客户端重连时凭 `match_id` 回到同一局。
- [ ] UE5 转写：`docs/ue5-client-model.md` §6 仍是未编译的形状指引。
- [ ] 机器人对手是权宜：真实对局里第 5 步是另一个客户端，`CLIENT_BOT` 只为演练存在。
- [ ] `by-match` 现在只按 `match_id` 唯一查；若将来一 match 可跨区域多 session，需要返回列表。