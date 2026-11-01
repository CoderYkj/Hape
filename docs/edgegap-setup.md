# Edgegap 接入指引（M2 第一家托管分配器）

> 这份文档回答两件事：**你在 Edgegap 控制台要点什么**、**后端要怎么配**。
> 第 1–4 节是你在网页上做的，第 5–7 节是代码与配置。文末有「需要你给我的东西」清单。

## 现状（对着真实账号核实过）

适配器已实现（`pkg/battlealloc/edgegap.go`），契约测试 10 条全绿。对着 live API 核实：

| 项 | 已核实的事实 |
|---|---|
| 认证头 | `Authorization: Token <token>` —— **不是** `Bearer`，裸 token 会 401 |
| 应用 / 版本 | `Hape` / `demo-app-lyra`（镜像 `edgegap/sample-unreal-5.5.4-lyra`） |
| 端口 | `gameport` = **7777/UDP** |
| 版本兜底 | `max_duration=60` 分钟、`req_cpu=1536`、`req_memory=3072`、`time_to_deploy=300s` |
| 套餐限额 | **同时只允许 1 个 active deployment** |
| 放置提示 | **必填其一**：`ip_list` / `location{lat,lon}` / filters。我们的适配器：有玩家 IP 用 IP，否则用配置的坐标 |
| 并发限额 | **1 台**。冒烟前先确认没有在跑的部署（`GET /v1/deployments`） |

**已跑通（2026-01-01）**：

```text
allocating Hape/demo-app-lyra for session smoke-...
READY in 3s: id=799a98c234f1 address=172.237.14.252:32719
released; nothing should be billing for this deployment
（随后 GET /v1/status/799a98c234f1 → Status.TERMINATED，deployments 列表归零）
```

`RUNTIME_TOKEN` 的注入**机制**已经验证：`ds/`（自有参考 DS 镜像）启动第一行就打印
`runtime_token=present(len=N)`，CI 的 `ds` job 在容器里跑完整一局并断言这一行，
还断言缺了这个变量时容器**拒绝启动**（spec 0012）。

仍未复验的是**Edgegap 这一侧**：`env_vars` 是否真的落进实例环境。做法很短：把发布的
`:<sha>-ds` 镜像在 Edgegap 建一个 app version，分配一次，看 container logs 里那行 banner。
触发条件：首次真实联调 / 第一次用我们自己的镜像分配。样例镜像（UE5 Lyra）不会打印，
所以在那之前 Edgegap 侧仍然只验到"API 接受了 env_vars"。

## 0. 为什么是 Edgegap

三家托管厂商里，GameLift 要先建 fleet、Agones 要先有 K8s 集群，而 Edgegap 是
"给一个容器镜像就能分配实例"，按秒计费、有免费额度。对小团队来说这是最短的路径，
也最符合本项目 `pkg/battlealloc.Allocator` 的抽象形状（一次分配 = 一次部署请求）。

## 1. 账号与令牌（你做）

1. 注册 <https://app.edgegap.com/>。
2. 控制台里创建 **API Token**（Account / Settings → API Tokens）。
   - 只给这个项目用，名字写清楚（例如 `hape-dev`）。
   - **不要**把它提交进仓库、贴进聊天或写进 compose 文件；用环境变量或 secret manager。
3. 记下 token，它就是我们后端要的 `BACKEND_EDGEGAP_TOKEN`。

## 2. 准备一个能跑的 DS 容器（你做）

Edgegap 分配的是**容器实例**，所以 UE5 Dedicated Server 必须先能被包成镜像：

- Linux Dedicated Server 构建（UE5 的 `Development Server` / `Shipping` + `-server`）。
- 一个 `Dockerfile`，暴露游戏端口（UE5 默认 DS 端口是 **7777/UDP**）。
- 推到任意 registry（Docker Hub 私有库 / GHCR / Edgegap 自己的 registry 都可以）。

> 这一项是真正的工作量所在，和我们的后端无关：后端只负责"申请一台"，不负责"造镜像"。

## 3. 在 Edgegap 建 Application（你做）

控制台 → **Applications → Create**：

| 字段 | 填什么 | 说明 |
|---|---|---|
| Application Name | `hape-ds`（自己定） | 就是后端要的 `BACKEND_EDGEGAP_APP` |
| Image | 你的仓库地址 + tag | 例：`ghcr.io/coderykj/hape-ds:0.1.0` |
| Ports | `7777/UDP`（游戏端口） | 若还需要查询端口（如 7778/UDP），一起加 |
| Resources | 按 DS 实际需求 | 先小后大，计费按资源 |
| Env / Args | 见第 4 节 | 让 DS 知道比赛 id、后端地址 |

创建后会自动产生一个 **Version**（就是镜像 tag 对应的那一版），
记下它的名字，作为 `BACKEND_EDGEGAP_VERSION`（也可以留空，Edgegap 会取最新版本）。

## 4. 给 DS 注入什么（和我们的契约对齐）

我们后端调用分配时会把比赛信息传下去，DS 启动后需要知道：

| 用途 | 建议的容器环境变量 | 我们的来源 |
|---|---|---|
| 本场 match/session | `MATCH_ID` / `SESSION_ID` | `battlealloc.Request.MatchID` / `SessionID` |
| 上报战报的后端地址 | `BACKEND_BASE_URL` | 部署注入 |
| 服务凭证 | `BACKEND_SERVICE_TOKEN` | **近期**是共享 token；M2 之后改 per-allocation token |

对应 Edgegap 的 `env_vars: [{key, value, is_hidden}]`——敏感值设 `is_hidden: true`。

## 5. API 形状（我们从这里做适配器）

Base URL：`https://api.edgegap.com`（以你控制台网络面板为准）。
认证：`Authorization: <token>`（Edgegap 直接把 token 放在这个头里，不加 `Bearer`）。
以下字段来自 Edgegap 官方 OpenAPI（`v1`）：

### 申请一台（Allocate）

```http
POST /v1/deploy
{
  "app_name": "hape-ds",
  "version_name": "0.1.0",          // 可省略 = 取最新版本
  "ip_list": ["1.2.3.4"],           // 可选：玩家 IP，用于就近选址
  "env_vars": [
    {"key": "MATCH_ID",   "value": "m_xxx"},
    {"key": "SESSION_ID", "value": "s_xxx"}
  ],
  "tags": ["hape-dev"]
}
```

响应里最关键的是 `request_id`（后续查状态、删实例都用它）。

### 查状态（Ready?）

```http
GET /v1/status/{request_id}
```

响应里的关键字段：

```text
request_id, current_status, running, fqdn, public_ip,
ports: { "7777": { internal, external, protocol, name, link } },
error, error_detail
```

- 就绪判据：`running == true`（同时看 `current_status`）。
- 失败判据：`error == true`，原因在 `error_detail`。
- **地址与端口**：用 `public_ip`（或 `fqdn`）+ `ports["7777"].external`
  → 对应我们 `battlealloc.Allocation{Address, Port}`。

### 回收（Release）

```http
DELETE /v1/stop/{request_id}
```

这一步直接对应钱：没停掉的实例按秒计费。我们已经在结算与超时两条路径上调
`Release`，接上之后就会真的省钱。

## 6. 后端配置（我这边做）

新增三个环境变量（与 `pkg/config` 同步登记）：

| 变量 | 例子 | 作用 |
|---|---|---|
| `BACKEND_ALLOCATOR` | `edgegap` | 选择适配器（现在会返回 `ErrNotConfigured`，接上后可用） |
| `BACKEND_EDGEGAP_TOKEN` | `xxxxx` | API Token，只在服务端 |
| `BACKEND_EDGEGAP_APP` | `hape-ds` | Application 名字 |
| `BACKEND_EDGEGAP_VERSION` | `0.1.0` | 可选；空 = 最新版本 |

适配器要满足 `Allocator` 的三个方法，语义我们已经有测试钉住：

- `Allocate` 必须带超时（建议 10s 内放弃）并区分可重试/不可重试错误：
  - 可重试：429、5xx、网络超时；
  - 不可重试：401/403（token 错）、404（app 不存在）、400（参数错）。
- `Release` 幂等：对已删除的 `request_id` 再删一次应当当成功（Edgegap 返回 404 时按成功处理）。
- 分配失败必须把 session 置 `failed`（已实现，见 `services/session/service.go`）。

## 7. 验证顺序（不烧钱）

1. **契约测试（不需要你的账号）**：用 `httptest` 假服务模仿 `/v1/deploy`、`/v1/status`、
   `/v1/stop` 的响应，断言我们的适配器解析正确、超时与错误分类正确。
   这一步我随时可以做，且会进 CI。
2. **真机冒烟（需要 token）**：先单独验证适配器，不要拉整条对局链：

   ```bash
   EDGEGAP_TOKEN=... EDGEGAP_APP=Hape EDGEGAP_VERSION=demo-app-lyra \
     go run ./tools/edgegap-check -timeout 5m
   ```

   它会分配一台、打印 `address:port`、**立刻释放**。成功输出形如
   `READY in 42s: id=abc123 address=1.2.3.4:31234` 后跟 `released`。
   想让它留着观察就加 `-keep`（会在日志里明确提示仍在计费）。
   确认适配器没问题后，再 `BACKEND_ALLOCATOR=edgegap` 起平台跑 `fakeds`。
3. **观察**：日志里应有 `session allocating`（带 provider=edgegap, address）与
   结算后的 release；数据库 `sessions.provider='edgegap'`。

## 8. 需要你给我的东西

按顺序，缺一个都做不下去：

1. **API Token**（`BACKEND_EDGEGAP_TOKEN`）——只给这一个项目用的。
2. **Application 名字**（`BACKEND_EDGEGAP_APP`）与**版本名**（可空）。
3. DS 容器的**端口协议**（UDP/TCP、7777 还是别的），这决定 `Allocation.Port` 取哪个映射。
4. 是否要按玩家 IP 就近选址（`ip_list`），还是先固定区域。

拿到 1–3 我就能写真适配器并以"契约测试 + 一次冒烟"交付；只有 1 没有 2、3 时，
我只能交付契约测试版本（标注未接真机）。

## 9. 已知风险

- **配额**：免费套餐只允许 **1 个并发部署**。冒烟前先确认没有别的部署在跑
  （`GET /v1/deployments`），否则会拿到 403「plan allows only 1 active deployments」——
  这是容量问题，适配器会把它分类为可重试，而不是 token 错误。
- **自动化测试不要对着真 API 跑循环**：契约测试用假服务，免费。
- **镜像拉取失败**：私有 registry 需要 Edgegap 侧的拉取凭据，首次部署常见失败原因是这个。
- **超时**：托管分配常在 20–90s 量级，客户端需要有"准备中"的 UI（`docs/ue5-ds-integration.md` 已写）。
- **协议细节以官方文档为准**：本文的字段名来自 Edgegap 官方 OpenAPI 镜像
  （<https://docs.edgegap.com/docs/api/dedicated-servers>），但**没有对着真 API 验证过**；
  接真机时以实际响应为准，不一致就改适配器与本文。