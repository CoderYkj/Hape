# 0012 - DS 运行时契约（自有镜像 + 每场凭证容器内验证）

- 状态：implemented
- 日期：2026-11-01
- 关联 ADR：0002（DS 是局内唯一权威）、0003（分配抽象）、0007（结算原子性）
- 前置：spec 0008（每场运行时凭证）
- 涉及：`ds/`、`Dockerfile.ds`、`tools/fakeds`、`.github/workflows/ci.yml`、`docs/ue5-ds-integration.md`

## 1. 背景

spec 0008 §7 留了一个未决项：`RUNTIME_TOKEN` 是否真的**到达实例内部**。

后端那一半已经验证过：`session.create` 生成凭证、Edgegap 的 `env_vars` 带上了它
（`is_hidden: true`，适配器契约测试断言）。但被分配的样例镜像是原版 UE5 Lyra 战斗服，
它既不打印环境变量，也没有"用自己这一场的凭证上报"的能力。于是"DS 双凭证"只验到了
**能拒**（缺 token → 401）这一半，**能过**那一半没有被任何东西证明过。

## 2. 目标 / 非目标

目标：

1. 一个自有 DS 镜像，在真实分配出来的实例里完成 `ready → 打一局 → 战报` 三个动作。
2. 启动一行日志证明"注入到位"，且**不打印凭证值**（日志里的 token 就是泄漏的 token）。
3. `result_id` 先落盘再上报，崩溃重启复用同一个值，一局只结算一次。
4. 以上在 CI 里可重复执行，覆盖容器形态与进程形态，并覆盖"缺少凭证"的失败形态。

非目标：

- **不做 UE5 构建**：没有引擎、没有许可、40 GB。`ds/` 是参考实现，UE5 真机替换的是
  **二进制**，不替换契约（环境变量名、两个上报接口、双凭证、幂等语义都不变）。
- 不实现游戏协议：UDP 只绑定并收包，不做帧同步/命中判定。
- 不改后端任何 op 的契约（`apigen -check` 无漂移）。

## 3. 环境契约

分配时由托管厂商注入，DS 只读：

| 变量 | 必需 | 说明 |
|---|---|---|
| `BACKEND_BASE_URL` | ✅ | 后端基址 |
| `BACKEND_SERVICE_TOKEN` | ✅ | 进程身份（共享） |
| `BATTLE_SESSION_ID` | ✅ | 这一场的会话 id |
| `RUNTIME_TOKEN` | ✅ | 这一场的凭证，**只进内存与请求头，不进日志** |
| `PLAYERS` | ✅ | JSON 数组，后端 `player_id`（不是 Nakama user id） |
| `MATCH_ID` / `MAP` | — | 透传进日志与战报 |
| `DS_PORT` | — | UDP 端口，默认 7777 |
| `MATCH_DURATION_SECONDS` | — | 默认 3 |
| `RESULTS_DIR` | — | `result_id` 落盘目录，默认 `Saved/Results` |
| `DS_ADVERTISE_ADDRESS` | — | 玩家实际拨的地址，默认 `127.0.0.1` |
| `DS_CRASH_BEFORE_REPORT` | — | 参考实现专用：`1` 时在上报前退出（退出码 17），供崩溃重启演练 |

**失败关闭**：缺任何一个必需变量 → 退出码 1，且错误信息逐个点名缺了哪些。
一个缺少凭证却照常开局的实例比不启动更糟：玩家打了，分不算。

## 4. 启动 banner

```text
ds: session=s_x match=m_y map="arena_01" players=2 duration=1s listen_port=7777 \
    runtime_token=present(len=43) service_token=present(len=11) base=http://... \
    results=/data/Saved/Results crash_before_report=false
```

只报**存在性与长度**。演练要证明的是"注入到位"，长度足够判断；把值打出来就变成了新的泄漏面
（容器日志通常对更多人有读权限）。

## 5. `result_id` 与崩溃重启

- 路径 `<RESULTS_DIR>/<session_id>.json`，临时文件 + rename（半截文件比没有文件更危险）。
- 字段：`session_id` / `result_id` / `created_at` / `reported_at`。
- 编号 `<session_id>-<unix>-<4 位随机>`：进程内唯一，落盘后可复现。
- **上报之前**落盘；上报成功后写 `reported_at`。重启的进程看到 `reported_at` 直接退出。
- 演练用退出码 **17**（刻意的崩溃）与 1（真实失败）区分开，脚本能分辨。

## 6. 上报重试语义

| 响应 | 含义 | DS 行为 |
|---|---|---|
| 200 `applied=true` | 首次结算成功 | 标记完成 |
| 200 `applied=false` | 同一 `result_id` 重放 | 标记完成（不是错误） |
| 400 | 字段不合法（players 与 session 不一致） | **不重试**，报错退出 |
| 401 | 凭证缺失/错误 | **不重试**，报错退出 |
| 409 | 该 session 已有另一份结果 | **不重试**，报错退出（DS 有 bug） |
| 412 | 状态不允许（ready→finished 之外） | 退避重试 |
| 503 / 5xx / 网络 | 下游扇出失败 | **同一个 `result_id`** 退避重试（1s→2s→4s，共 4 次） |

`ready` 只在 5xx/网络时重试：412 说明会话已经 finished/failed，重试不会改变答案。

## 7. 验证

| 层 | 用例 |
|---|---|
| 单测（`ds/`） | 缺变量报出每个名字；默认值；`PLAYERS` 非法/少于 2 人；端口越界 |
| 单测（banner） | 断言 banner 含 `len=N`、**不含凭证值** |
| 单测（落盘） | 重启复用同一 `result_id`；`reported_at` 持久化；不残留 `.tmp`；拒绝路径穿越的 session id |
| 单测（客户端） | 双凭证都带上；503/412 重试；409/400 不重试；重放 `applied=false` 不算错；4 次后放弃 |
| CI `ds` job | 镜像非 root（uid 10001）；**缺 `RUNTIME_TOKEN` 必须启动失败并点名该变量**；容器内真打一局；进程形态再打一局；崩溃重启后只结算一次 |
| 本机实跑 | `-launch-ds exec` 跑通（banner `runtime_token=present(len=43)`、`applied=true`、`session finished`、金币 +100 / XP +125 / 评分 +24）；`-crash-restart` 两次 launch 复用同一 `result_id`；缺 token 退出码 1 |

## 8. 回滚

删掉 `ds/`、`Dockerfile.ds`、CI 的 `ds` job 即可；后端零改动，`fakeds` 默认角色行为不变。

## 9. 未决

- [ ] **Edgegap 真机复验**：机制已在容器里验证（CI + 本机），但"Edgegap 的 `env_vars`
  真的落进实例环境"还没在 Edgegap 上复验。做法：把 `:<sha>-ds` 镜像推上去，在 Edgegap
  建一个指向它的 app version，分配一次，看 container logs 里那行 banner。
- [ ] UE5 实现替换参考实现（契约不变，只换二进制）。届时 C++ 侧的 `LoadOrCreateResultId`
  逻辑要照抄第 5 节。
- [ ] `PLAYERS` 是参考实现的权宜：真实 UE5 DS 从玩家 join 事件得知名单，env 只给会话身份。