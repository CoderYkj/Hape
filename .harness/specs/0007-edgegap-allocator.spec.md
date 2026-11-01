# 0007 - Edgegap 分配器

- 状态：implemented（真机分配 + 释放成功：`READY in 3s` → `172.237.14.252:32719` → 已 TERMINATED）
- 日期：2026-01-01
- 关联 ADR：0003（分配器抽象）、0007（结算事务）
- 涉及：`pkg/battlealloc`（新增 `edgegap.go` + 错误分类）、`pkg/config`、`platform`、
  `tools/edgegap-check`

## 1. 背景

`pkg/battlealloc` 之前只有 `local` 与三个返回 `ErrNotConfigured` 的桩。M2 的第一家
托管厂商选 Edgegap：不需要先建 fleet，也不需要 K8s，给镜像就分配。

## 2. 目标 / 非目标

目标：

1. 实现 `Allocator` 三方法（`Provider`/`Allocate`/`Release`），不泄漏厂商类型。
2. `Allocate` 有超时、有轮询、失败分类明确（可重试 / 永久）。
3. 放弃的部署必须被停掉（超时、错误、端口缺失），不能留给账单。
4. 契约测试覆盖所有分支；真机只做一次"分配 + 立即释放"。

非目标：

- GameLift / Agones 适配器。
- per-allocation token（独立一项）。
- 玩家 IP 的真实采集（`Request.IPs` 已支持透传，采集端在 matchmaking/session，未接）。

## 3. 核实过的真实事实（不是文档推断）

对着 live API 调用后确认：

| 项 | 值 |
|---|---|
| 认证头 | `Authorization: Token <token>`（**不是** `Bearer`；裸 token 会被 401） |
| 部署 | `POST /v1/deploy` → `request_id` |
| 状态 | `GET /v1/status/{request_id}` → `running`/`public_ip`/`fqdn`/`ports{}`/`error` |
| 回收 | `DELETE /v1/stop/{request_id}`；已停过返回 404 |
| 应用 | `Hape`，active |
| 版本 | `demo-app-lyra`，镜像 `edgegap/sample-unreal-5.5.4-lyra`、tag `25.05.30-16.45.19-04` |
| 端口 | `gameport` = **7777/UDP**，`to_check: true` |
| 版本参数 | `max_duration=60`（分钟，厂商侧兜底）、`req_cpu=1536`、`req_memory=3072`、`time_to_deploy=300s` |
| 套餐限额 | **同时只允许 1 个 active deployment** |
| 放置提示 | **必填其一**：`ip_list` / `geo_ip_list` / `location{lat,lon}` / `filters`。都没有 → 400「You must specify at least ip_list, geo_ip_list, a valid location or use filters」 |

## 4. 实现要点

- 轮询直到 `running==true`；`error==true` 立即失败并把 `error_detail` 带进错误。
- 端口选择：按 name（`gameport`）→ 按 internal+protocol（7777/UDP）→ 任意 UDP → 任意，
  取 `external`。找不到可用端口是永久错误，并且先停掉部署。
- 地址：`public_ip`，空则用 `fqdn`。
- 错误分类：
  - 可重试：429、5xx、网络错误、**并发额度用满的 403**、超时；
  - 永久：401、token 被拒的 403、400、404、端口缺失。
- 403 的两种含义靠响应体区分（`active deployment`/`reached this limit`/`upgrade your tier`
  → 容量问题；否则按 token 被拒）。
- `Release` 幂等：404 视为成功（目标状态已达成）。

## 5. 验证

| 层 | 用例 |
|---|---|
| 契约（`httptest` 假服务，10 条） | 正常分配（含 env/ip_list/version 透传）、FQDN 回退、端口优先级、部署报错后停止、超时后停止、403 两种含义、状态读取重试、Release 幂等三态、缺配置 |
| 真机（live API） | 认证头、端点、应用/版本/端口读取 **已核实**；403 并发限额、400 缺放置提示两次失败都被修掉 |
| 真机成功路径 | `go run ./tools/edgegap-check -lat 31.23 -lon 121.47` → `READY in 3s`、`172.237.14.252:32719`、释放后状态 `TERMINATED`、部署列表归零 |
| 仍未确认 | `RUNTIME_TOKEN` 是否真的出现在**容器内**环境（API 接受了 `env_vars`，但 DS 是 UE5 Lyra，不会打印它） |

## 6. 回滚

`BACKEND_ALLOCATOR=local` 即回到本地分配器；适配器是新增文件，删除不影响其它域。

## 7. 未决

- [x] 真机分配的一次成功验证 → **已完成**（`READY in 3s`，释放后 `TERMINATED`）
- [ ] 玩家 IP 采集与透传（`Request.IPs` 已就绪，采集端未接）
- [x] 厂商侧回收回读确认 → **已实现**（spec 0015）：`Release` 读回 `running == false`/404
  才算成功，未确认的进 `sessions.release_pending` 由 reconciler 重试。真实的
  `current_status` 时序仍需一次真实释放复验。
- [ ] 是否在 CI 里跑 Edgegap 契约测试（现在跟着 `go test ./...` 跑，不花钱）