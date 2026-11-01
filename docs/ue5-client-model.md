# 客户端侧极简模型（UE5 对照）

> 这份文档描述**客户端那一半**要做什么，以及一份可直接运行的最小实现
> （`clients/minimal`，CI 里对着真实 Nakama + 后端跑）。
> UE5 代码片段**没有在引擎里编译过**——它是转写指引，不是已验证实现。

## 1. 客户端只做四件事

```text
  ① 登录 Nakama         → 拿到 Nakama session token（身份）
  ② 调 RPC login        → 拿到后端 player_id + 后端 token（兑换）
  ③ 用后端 token 调后端  → 读档案、排队、轮询（一切游戏行为）
  ④ 断线重连            → 同一套 token 重放 ③，不重新 ①
```

三条边界，越界就是设计错误：

| 客户端**绝不能**做的事 | 为什么 |
|---|---|
| 上报战报（`/sessions/{id}/results`） | 只有 DS 能，且要每场 `X-Session-Token`（spec 0008） |
| 弹出匹配队列（`/matchmaking/queues/{q}/pop`） | 需要 service token，那是服务器的事 |
| 自己给自己发货币/经验 | 后端只认 DS 战报衍生的幂等键 |

## 2. 逐步契约

| 步 | 请求 | 拿到什么 |
|---|---|---|
| ① | `POST {nakama}/v2/account/authenticate/device?create=true`，`Authorization: Basic base64(<serverKey>:)` | `token`（Nakama session）+ `user.id` |
| ② | `POST {nakama}/v2/rpc/login?unwrap=true`，`Authorization: Bearer <Nakama token>` | 后端 `/v1/account/sessions` 的原文：`player.id` + `session_token`（生产环境是后端签发的 JWT，`iss=gamebackend`） |

> **`/v1/account/sessions` 不是客户端接口**：它是服务专属（ADR 0010），由 Nakama 插件
> 用服务凭证调用。客户端拿到 token 的唯一途径是 ② 这个 RPC。
| ③ | `GET {backend}/v1/account/players/{player_id}`，`Authorization: Bearer <backend token>` | 自己的档案 |
| ④ | `POST {backend}/v1/matchmaking/tickets` `{"queue":"ranked_1v1"}` | `ticket.id`（`status=waiting`） |
| ⑤ | `GET {backend}/v1/matchmaking/tickets/{id}` | 票据状态；匹配后被 `matched`，出现 `match_id` |

`docs` 里的后端契约以 `api/openapi/*.json` 为准（那是生成的，不会漂移）。

## 3. Token 规则（最容易写错的地方）

| 规则 | 说明 |
|---|---|
| 两个 token 用途不同 | Nakama token 只用来调 Nakama（含 RPC）；后端 token 只用来调后端 |
| 都放内存，**不要写日志** | 日志系统会把它们带进崩溃上报 |
| Nakama token 会过期（默认 7200s） | 过期 → 重新 ①，然后 ② 拿新的后端 token；后端 token 不单独续期 |
| 后端 token 失效（401） | 视为"需要重新兑换"：重放 ②，不要重放 ① |
| 不要缓存 player_id 而跳过 ② | ② 是幂等的（`external_id` 唯一），重放安全 |

## 4. 状态机（客户端只需要这四个状态）

```text
Disconnected ──① 成功──► Authenticated ──② 成功──► Ready
     ▲                        │                     │
     │                    ①/② 失败                  ④ 入队成功
     └────────── 401/超时 ────┴─────────────────────► InQueue ──⑤ matched──► Ready(对局)
```

## 5. 错误 → UI 的最小映射

| HTTP | code | 客户端动作 |
|---|---|---|
| 401 | `unauthenticated` | 重走 ① + ②；连续两次失败 → 提示"登录失败" |
| 403 | `permission_denied` | 不是自己的资源；不要重试，上报日志 |
| 412 | `failed_precondition` | 业务拒绝（余额不足、状态非法）：展示原因，不重试 |
| 409 | `conflict` | 幂等冲突：刷新状态即可（例如已有 waiting 票） |
| 503 | `unavailable` | 可重试：指数退避（1s/2s/4s，上限 30s） |
| 其它 | — | 展示"服务异常"，保留 token 以便重试 |

## 6. UE5 转写指引（**未编译**）

`clients/minimal` 是本模型的 Go 实现，逐行对应上面的五步。UE5 侧的对应关系：

| Go（已验证） | UE5（未验证） |
|---|---|
| `http.NewRequest` + `client.Do` | `FHttpModule::Get().CreateRequest()` + `ProcessRequest()` |
| `json.Marshal/Unmarshal` | `TSharedPtr<FJsonObject>` + `FJsonSerializer` |
| 结构体 | `USTRUCT` + `FJsonObjectConverter` |
| `env("NAKAMA_HTTP", ...)` | `DefaultEngine.ini` 里的 `[/Script/Game.Nakama]` 配置节 |
| `fmt.Printf` 步进日志 | `UE_LOG(LogNakama, Log, TEXT("step ..."))` |

最小骨架（**不要当成可编译代码**）：

```cpp
// 未编译、未在引擎里跑过。这是形状，不是实现。
void UBackendClient::Login(const FString& DeviceId)
{
    // ① Nakama device auth
    auto Req = FHttpModule::Get().CreateRequest();
    Req->SetURL(NakamaBase + TEXT("/v2/account/authenticate/device?create=true"));
    Req->SetVerb(TEXT("POST"));
    Req->SetHeader(TEXT("Content-Type"), TEXT("application/json"));
    Req->SetHeader(TEXT("Authorization"),
        TEXT("Basic ") + FBase64::Encode(ServerKey + TEXT(":")));
    Req->SetContentAsString(FString::Printf(TEXT("{\"id\":\"%s\"}"), *DeviceId));
    Req->OnProcessRequestComplete().BindLambda(
        [this](FHttpRequestPtr, FHttpResponsePtr Resp, bool bOk) {
            const FString NakamaToken = JsonField(Resp, TEXT("token"));
            ExchangeForBackendToken(NakamaToken);   // ②
        });
    Req->ProcessRequest();
}
```

## 7. 怎么验证

| 层 | 方式 |
|---|---|
| 协议本身 | `go run ./clients/minimal`（CI 的 `nakama` job 里跑，对着真实 Nakama + 后端） |
| 断言 | 客户端自己打印 `CLIENT_OK=1`；CI 另外断言 `players` 表里存在该 `external_id` |
| UE5 转写 | **未验证**：需要 UE5 工程编译 + 一次真机联调 |

## 8. 明确不做

- 不做断线重连的会话恢复（需要一局内的连接状态，DS 侧才有）
- 不做 Nakama 好友/公会/聊天（不在本模型范围）
- 不做客户端预测/回滚（局内由 DS 权威）