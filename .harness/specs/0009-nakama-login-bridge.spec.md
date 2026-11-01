# 0009 - Nakama 登录桥接（Lua 运行时模块）

- 状态：implemented（CI 在真 Nakama 上验证）
- 日期：2026-01-01
- 关联 ADR：0009（Go 插件 vs Lua 的决策）、0001（身份归属）
- 涉及：`nakama/runtime/login.lua`、`deploy/docker-compose.yml`、`tools/nakama-smoke`、CI job `nakama`

## 1. 背景

玩家登录链路是：客户端 → Nakama（身份）→ **桥接 RPC** → 后端 `POST /v1/account/sessions`
→ 后端 player id + 会话 token。桥接层曾是 Go 插件，因 Go plugin ABI 与镜像构建路径
强耦合而无法对齐（ADR 0009 有实测数据），改为 Lua 模块。

## 2. 契约

```text
POST /v2/rpc/login?unwrap=true      Authorization: Bearer <Nakama session>
→ 200  <后端 /v1/account/sessions 的响应原文>
      {"player": {"id": "p_...", "external_id": "<Nakama user id>", ...},
       "session_token": "..."}
```

失败映射：

| 情况 | RPC 返回 |
|---|---|
| ctx 里没有用户 | `{"code":16,...}` → HTTP 401 |
| 后端不可达 / 非 200 | `{"code":14,...}` → HTTP 503，消息里带 URL 与原因 |
| 后端返回空体 | `{"code":13,...}` → HTTP 500 |

## 3. 实现要点

- `nakama/runtime/login.lua`，约 90 行，`nk.register_rpc(rpc_login, "login")`，
  加载时打一行 `gamemodule loaded: rpc=login (lua)` 供 CI 断言。
- 后端地址默认 `http://backend:8080`（compose 服务名），
  可用 runtime env `backend_base_url` / `backend_service_token` 覆盖。
- `nk.http_request` 的实际调用形式是 `(url, "POST", headers, body, timeout)`，
  返回 `(code, headers 表, body 字符串)`；两个细节都在注释里。
- 不吞错误：Lua 的 pcall 异常文本会进 RPC 的错误消息，客户端能直接读到原因。

## 4. 验证

| 层 | 内容 |
|---|---|
| CI job `nakama` | 起 compose 全套 → 迁移 → 断言 `gamemodule loaded` → `tools/nakama-smoke`（设备登录 → RPC → 后端读取）→ 断言 `players` 表有该 `external_id` 的行 |
| 探针 | `tools/nakama-smoke` 输出 `EXTERNAL_ID=` / `PLAYER_ID=` 供 shell 断言；失败时把关键行写成 GitHub annotation |
| 实测结果 | run #29 全绿：`rpc login ok: player=p_692b1b1aaabc external_id=9595ba11-...` |

## 5. 未验证

- **客户端侧**：UE5/浏览器怎么拿 Nakama session、再调 RPC、再用返回的后端 token 调后端
  （服务端链路已验证，客户端代码尚未落地）。
- Nakama console / 好友 / 公会等社交功能未接（不在本 spec 范围）。
- `nakama/modules`（Go 插件）保留为备选路径，ABI 未解决。

## 6. 回滚

删除 `nakama/runtime/login.lua` 并把 compose 挂载改回 `../nakama/build` 即回到 Go 插件路径
（该路径未验证）。桥接契约不变，后端无改动。