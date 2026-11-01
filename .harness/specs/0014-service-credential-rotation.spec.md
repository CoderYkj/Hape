# 0014 - 服务凭证轮换（多凭证并行 + 调用方身份）

- 状态：implemented
- 日期：2026-11-01
- 关联 ADR：0010（登录收口与玩家 token）；README「已知缺口」里的共享静态 token 一栏
- 涉及：`pkg/httpx`（`ServiceCredentials`）、`pkg/config`、`platform`、`cmd/platform`、`deploy/*`

## 1. 背景

服务间认证一直是**一个共享静态 token**（`BACKEND_SERVICE_TOKEN`）。两个问题：

1. **换不了**：换 token 意味着同时改后端、Nakama 插件、定时任务、运维脚本，
   任何一处没跟上就是 401。于是它实际上永远不会被换——这正是长期密钥的典型死法。
2. **不知道是谁**：日志里只有一个 `service=session` 之类的 op 归属，没有调用方身份。
   "这条奇怪的对局是谁建的" 无法回答。

（第三个问题——**一条凭证能调所有 service 接口**——本轮不做，见 §9。）

## 2. 目标 / 非目标

目标：

1. 同时接受多条凭证，每条有一个**名字**。
2. 认证结果带上名字（`Principal.Service`），请求日志打出 `caller` 字段。
3. 零停机轮换：加新的 → 迁移调用方 → 删旧的，中间不需要停机窗口。
4. 启动期校验：重复名字、重复 token、非法名字直接拒绝启动。

非目标：

- 不做按调用方的**最小权限**（每条凭证能调哪些 service）。见 §9。
- 不做 mTLS、不做凭证哈希落盘、不做自动过期。

## 3. 设计

```text
BACKEND_SERVICE_CREDENTIALS="nakama=<secret-a>,worker=<secret-b>"
   ↓ httpx.ParseServiceCredentials（启动期解析，格式错误直接退出）
ServiceCredentials[{Name:"nakama",Token:"..."},{Name:"worker",Token:"..."}]
   ↓ 注入 DevAuth / JWTAuth（dev 与 prod 用同一套查找逻辑）
Authorization: Bearer <secret> → Lookup → Principal{Service:"nakama"}
   ↓
请求日志：{"op":"account.login","caller":"nakama",...}
```

规则：

| 规则 | 理由 |
|---|---|
| 名字限制 `^[a-z][a-z0-9_-]{0,31}$` | 名字进日志与看板，不能是任意字符串 |
| token 不允许含逗号 | 语法分隔符；base64url 本身不含逗号 |
| 重复名字 / 重复 token 拒绝启动 | 两个名字共享一个密钥会让"吊销一条"变成"吊销两条" |
| 查找遍历全部并**不提前返回** | 耗时不能透露哪条匹配；比较本身是常数时间 |
| `BACKEND_SERVICE_CREDENTIALS` 与 `BACKEND_SERVICE_TOKEN` **互斥** | 轮换是"在集合里加一条"，不是"把旧的变量留着"；合并会静默继续接受操作者以为已退役的凭证 |
| dev 模式默认 `service=dev-service` | `go run ./cmd/platform` 保持零配置，本机工具不用改 |

## 4. 轮换流程（零停机）

```bash
# 1) 加新凭证，旧的不动。此时两条都有效。
BACKEND_SERVICE_CREDENTIALS="nakama=<old>,nakama-v2=<new>"
# 2) 重新部署后端，确认启动日志列出两个名字：
#    auth selected mode=jwt service_credentials=[nakama nakama-v2]
# 3) 逐个把调用方换成新凭证（Nakama 插件的 BACKEND_SERVICE_TOKEN、定时任务等）。
#    用日志确认切换完成：不再出现 caller=nakama。
# 4) 去掉旧凭证，再部署一次。旧凭证立刻 401。
BACKEND_SERVICE_CREDENTIALS="nakama-v2=<new>"
```

第 3 步的判据只看日志：`caller` 字段是这条链路唯一的"谁在用哪个凭证"的证据。
这也是为什么名字要稳定、可读——它不是装饰。

## 5. 失败模式

| 场景 | 结果 |
|---|---|
| 两个变量都设置 | 启动失败，报错说明二者互斥并指向正确做法 |
| 格式不是 `name=token` | 启动失败，报出出错的那一项 |
| 重复名字或重复 token | 启动失败 |
| 已被移除的凭证 | 401 `unauthenticated`（没有"最后一分钟宽限"这种模糊语义） |
| prod 未配置任何凭证 | 启动失败（dev auth 关闭时至少需要一组） |

## 6. 验证

| 层 | 用例 |
|---|---|
| `pkg/httpx` | 解析成功/失败用例；空串视为未配置；查找命中每一条；`Validate` 拦住直接构造的重复集合；名字排序 |
| `pkg/httpx`（JWT） | 重叠期两条都通过且各自带回名字；移除旧的后旧 401、新的仍通过；启动期拒绝重复 token |
| `pkg/config` | 两个变量同时设置被拒；只配凭证集能通过；都不配且关闭 dev auth 被拒 |
| `platform` | dev 模式下两条凭证都能登录，未配置的 `dev-service` 被拒 401 |
| 日志 | 请求日志新增 `caller` 字段（由 `Principal.Service` 提供） |

## 7. 回滚

删掉 `BACKEND_SERVICE_CREDENTIALS`、改回 `BACKEND_SERVICE_TOKEN` 即可——单条凭证会被
包装成名为 `service` 的一条，行为与之前完全一致。

## 8. 运维注意

- 凭证不写进仓库、不贴进聊天；`deploy/systemd/backend.env.example` 只放占位符。
- 换下来的 token 不要再留在一个"以防万一"的文件里；那等于没退役。
- 本机/CI 的 `dev-service` 是**开发凭据**，prod 拒绝 dev auth 时它一文不值。

## 9. 未决（明确列出，避免误以为已完成）

- [ ] **按调用方的最小权限**：现在任何一条凭证都能调所有 service-auth op。做法是给
  `ServiceCredential` 加 `Services []string`（`*` 允许全部），在 `Router` 里拿 op.Service
  比对。阻碍：需要先确定每个调用方真实需要的 service 列表，否则就是把 dev 工具也锁死。
- [ ] mTLS 或短期令牌（凭证仍然长期有效，只是能换了）。
- [ ] 轮换的可观测性：目前靠人看日志，没有"旧凭证仍在使用"的告警指标。