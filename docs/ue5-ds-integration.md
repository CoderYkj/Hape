# UE5 Dedicated Server 接入指南

> 读者：实现战斗服的人 · 状态：活文档（实现指南）· 最后更新：2026-11-03 · 上游 spec 0012
>
> **角色分工**：要签的约定在 `docs/ds-runtime-contract.md`；本文讲**怎么实现**它
> （C++ 形状、厂商差异、排障）。两者冲突时以契约为准。

> 本文里的 C++ 片段是**示例**，没有在 UE5 工程里编译验证。命令与 HTTP 契约是经过验证的
> （`e2e/flow_test.go` 与 `tools/fakeds` 实际跑通）。

## 1. DS 需要做的三件事

| # | 时机 | 动作 | 后端接口 |
|---|---|---|---|
| 1 | `Listen` 成功之后 | 上报地址端口 | `POST /v1/session/sessions/{id}/ready` |
| 2 | 每局开始 | 生成并**持久化** `result_id` | （无，本地落盘） |
| 3 | 对局正常结束 | 上报权威战报 | `POST /v1/session/sessions/{id}/results` |

两个上报都要带**双凭证**：

```text
Authorization: Bearer <BACKEND_SERVICE_TOKEN>    # 进程身份
X-Session-Token: <RUNTIME_TOKEN>                 # 这一场的身份
```

`RUNTIME_TOKEN` 由后端分配时生成，通过托管厂商的实例环境变量注入（Edgegap 走
`env_vars`，`is_hidden: true`）。DS 把它读进内存、每个上报请求都带上即可，不要写日志。
缺它或者拿别场的 token → `401 unauthenticated`（见 spec 0008）。

**可执行版本**：`ds/` 里就是这个契约的参考实现（Go，不是 UE5），
`ds/README.md` 写了环境变量表与本地跑法，spec 0012 写了为什么这么定。
UE5 真机替换的是那个二进制，接口与变量名不变。

第 2 条最容易被忽略：`result_id` 必须是"这一局的结果标识"，DS 崩溃重启后仍能拿回同一个值，
否则重试就会变成"两次结算"。建议：`<session_id>-<开始时间戳 utc>-<随机短码>`，写进
`Saved/Results/<session_id>.json`，上报成功后标记完成。

## 2. DS 需要的环境/启动参数

**完整清单不在这里**：变量名、必填性、默认值、失败行为都以
`docs/ds-runtime-contract.md` §1 为准（那是引擎组要签署的契约）。本节只补充
**实现层面的来源差异**。

`BATTLE_SESSION_ID` 的来源取决于托管厂商：

- **GameLift**：`CreateGameSession` 时把 session id 放进 `GameSessionData`，DS 从 `GameLiftServerSDK` 读回。
- **Edgegap**：deployment request 的 `metadata` 里带上，DS 读环境变量。
- **Agones**：`GameServer.Spec.Parameters` 或 `GameServerAllocation.Metadata`。

## 3. 上报 ready（示例）

```cpp
// DsBackendClient.cpp —— 示例，未在本仓库编译
void UDsBackendClient::ReportReady(const FString& SessionId, const FString& Address, int32 Port)
{
    const FString Url = FString::Printf(
        TEXT("%s/v1/session/sessions/%s/ready"), *BackendBaseUrl, *SessionId);

    const TSharedRef<FJsonObject> Body = MakeShared<FJsonObject>();
    Body->SetStringField(TEXT("address"), Address);
    Body->SetNumberField(TEXT("port"), Port);

    FString Payload;
    const TSharedRef<TJsonWriter<>> Writer = TJsonWriterFactory<>::Create(&Payload);
    FJsonSerializer::Serialize(Body, Writer);

    TSharedRef<IHttpRequest> Request = FHttpModule::Get().CreateRequest();
    Request->SetURL(Url);
    Request->SetVerb(TEXT("POST"));
    Request->SetHeader(TEXT("Content-Type"), TEXT("application/json"));
    Request->SetHeader(TEXT("Authorization"), FString::Printf(TEXT("Bearer %s"), *ServiceToken));
    Request->SetContentAsString(Payload);
    Request->OnProcessRequestComplete().BindUObject(this, &UDsBackendClient::OnReadyResponse);
    Request->ProcessRequest();
}
```

要点：

- 调用时机在 `UWorld::BeginPlay` 之后、`Listen` 成功之后（`NetDriver->IsServer()` 为真）。
- 失败重试：2s → 4s → 8s，最多 5 次；仍失败就**不要**让玩家进（否则这一局无法结算）。

## 4. 上报战报（示例）

```cpp
void UDsBackendClient::ReportResult(const FString& SessionId, const TArray<FPlayerOutcome>& Outcomes)
{
    // result_id 必须来自本地持久化，不能每次现生成
    const FString ResultId = LoadOrCreateResultId(SessionId);

    const TSharedRef<FJsonObject> Body = MakeShared<FJsonObject>();
    Body->SetStringField(TEXT("result_id"), ResultId);
    Body->SetNumberField(TEXT("duration_seconds"), MatchDurationSeconds);

    TArray<TSharedPtr<FJsonValue>> Players;
    for (const FPlayerOutcome& Outcome : Outcomes)
    {
        const TSharedRef<FJsonObject> Entry = MakeShared<FJsonObject>();
        Entry->SetStringField(TEXT("player_id"), Outcome.BackendPlayerId);
        Entry->SetBoolField(TEXT("won"), Outcome.bWon);
        Entry->SetNumberField(TEXT("score"), Outcome.Score);
        Entry->SetNumberField(TEXT("kills"), Outcome.Kills);
        Entry->SetNumberField(TEXT("deaths"), Outcome.Deaths);
        Entry->SetNumberField(TEXT("damage"), Outcome.Damage);
        Players.Add(MakeShared<FJsonValueObject>(Entry));
    }
    Body->SetArrayField(TEXT("players"), Players);
    // …POST /v1/session/sessions/{id}/results
}
```

重试语义（每种响应该做什么）见 `docs/ds-runtime-contract.md` §4，那是唯一权威版本；
本节不再复述，避免两份表各自漂移。

`players` 必须与后端 session 的 `players` 完全一致：用后端返回的 `player_id`，
不是 Nakama user id，也不是客户端传入的任何 id。

## 5. 客户端连接流程

```text
1. 客户端 POST /v1/matchmaking/tickets              → ticket
2. 客户端轮询 GET /v1/matchmaking/tickets/{id}      → 拿到 match_id / matched
3. （服务端 worker）POST /v1/matchmaking/queues/{q}/pop → session.create
4. 客户端轮询 GET /v1/session/sessions/{id}
       state=allocating → 显示"准备中"
       state=ready      → 拿到 address/port，OpenLevel 连接
       state=finished   → 结算页
5. 连接：<address>:<port>（v0 直连，无 relay）
```

第 4 步用的就是 `GET /v1/session/sessions/by-match/{match_id}`（spec 0013）：票据只给
`match_id`，这个读接口把它换成 `address:port`。它之前是缺的——客户端因此进不了对局。
只返回名单内玩家能读的那一场，且永远不含 `runtime_token`。

## 6. 没有 UE5 时怎么联调

```bash
go run ./cmd/platform                       # 终端 1
go run ./tools/fakeds                       # 终端 2：本进程直接扮演 DS
go run ./tools/fakeds -launch-ds exec       # 扮演"厂商"：启动 ds/ 二进制并注入环境
go run ./tools/fakeds -launch-ds docker -ds-image hape-ds:ci   # 容器形态
go run ./tools/fakeds -launch-ds exec -crash-restart           # 崩溃重启，只结算一次
```

`tools/fakeds` 默认角色就是本文档的可执行版本（第 1、2、4 节的动作全做一遍）。
`-launch-ds` 把"厂商"与"战斗服"分开：先分配，再把环境注入一个**独立进程/容器**，
由它自己上报。这条路径才是"凭证真的到了实例里"的证明。

## 7. 排障

| 现象 | 先看什么 |
|---|---|
| DS 上报 401 | 请求里的服务 token 是否在后端凭证集里（`BACKEND_SERVICE_CREDENTIALS` 的某个 token） |
| DS 上报 403 | `permission_denied`：token 是真的，但那条凭证的 scope 没覆盖 `session`。给该凭证加 `:session`（spec 0016） |
| DS 上报 404 | `BACKEND_BASE_URL` 路径前缀是否多/少了 `/v1` |
| DS 上报 401 | 是不是漏了 `X-Session-Token`，或拿成了别场的 token；会话没有 token 说明是旧数据 |
| DS 上报 412 | `GET session` 看状态是不是还在 `allocating`（ready 没成功） |
| 玩家结算了但没到账 | 后端日志按 `result_id` grep，看扇出三条是否都成功 |
| 同一局结算两次 | DS 的 `result_id` 是否每次现生成（这是 bug） |
