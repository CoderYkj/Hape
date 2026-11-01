# ds/ — 参考 Dedicated Server

这个目录是一个**可运行的参考战斗服**，不是 UE5 构建产物。它实现了
`docs/ue5-ds-integration.md` 要求 DS 做的全部动作（读注入环境 → 监听 → 上报 ready →
打一局 → 上报权威战报，且只结算一次），所以整条对局链路可以在容器里、带着真实的
每场凭证跑通，而不用先有引擎。

UE5 真机上线时替换的是**这个二进制**，不替换契约：环境变量名、两个上报接口、
双凭证、`result_id` 幂等语义都保持不变（spec 0012）。

## 环境契约

分配时由托管厂商注入，进程只读不写：

| 变量 | 必需 | 说明 |
|---|---|---|
| `BACKEND_BASE_URL` | ✅ | 后端基址 |
| `BACKEND_SERVICE_TOKEN` | ✅ | 进程身份（共享） |
| `BATTLE_SESSION_ID` | ✅ | 这一场的会话 |
| `RUNTIME_TOKEN` | ✅ | 这一场的凭证；**只上报，不打日志** |
| `PLAYERS` | ✅ | JSON 数组，后端 `player_id`（不是 Nakama user id） |
| `MATCH_ID` / `MAP` | — | 透传进日志与战报 |
| `DS_PORT` | — | UDP 端口，默认 7777 |
| `MATCH_DURATION_SECONDS` | — | 默认 3 |
| `RESULTS_DIR` | — | `result_id` 落盘目录，默认 `Saved/Results` |
| `DS_ADVERTISE_ADDRESS` | — | 玩家拨的地址，默认 `127.0.0.1` |
| `DS_CRASH_BEFORE_REPORT` | — | `1` 时在**上报前**退出（退出码 17），用于崩溃重启演练 |

启动第一行是这套东西存在的意义：

```text
ds: session=s_x match=m_y map="arena_01" players=2 duration=1s listen_port=7777 \
    runtime_token=present(len=43) service_token=present(len=11) base=... results=... crash_before_report=false
```

**只报长度不报值**：证明"注入到位"不需要把凭证写进日志，写进去就是泄漏。

## 为什么它对收到的包做回声

参考实现不实现任何游戏协议。但"客户端真的连上了战斗服"这件事需要一个**双向**证据，
所以它把收到的每个包原样回给来源地址，客户端据此断言"我发了 N 个，收回 N 个"。
UE5 真机这里换成正常的复制与输入处理。

## 结算只发生一次

`result_id` 在**上报之前**落到 `RESULTS_DIR/<session_id>.json`（临时文件 + rename），
重启后读回同一个值。上报成功后写入 `reported_at`，重启的进程看到它就直接退出。

崩溃重启演练（`fakeds -crash-restart`）就是验这一条：第一次跑在报战报前崩掉，
第二次跑复用同一个 `result_id`，后端 `applied=true` 只出现一次。

## 自己跑

```bash
# 终端 1
go run ./cmd/platform

# 终端 2：以"厂商"的身份分配一场，并启动这个参考 DS
go run ./tools/fakeds -launch-ds exec

# 容器形态（CI 里跑的就是这个）
docker build -f Dockerfile.ds -t hape-ds:ci .
go run ./tools/fakeds -launch-ds docker -ds-image hape-ds:ci

# 崩溃重启：结果只结算一次
go run ./tools/fakeds -launch-ds docker -crash-restart
```

本机没有 Docker 时用 `-launch-ds exec`，走的是同一条链路（同一套环境变量、
同一个二进制）；Docker 路径由 CI 的 `ds` job 覆盖。