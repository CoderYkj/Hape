# 0020 - 收尾八项：最小权限到 op、入站限流、分页、两个计数、归档、客户端重登

- 状态：implemented
- 日期：2026-11-03
- 关联：spec 0016（service 级 scope）、0014（凭证轮换）、0018（台账）、0019（观测）、0011（玩家 token）
- 涉及：`pkg/httpx`（credentials/router/ratelimit）、`pkg/api`（查询参数）、`tools/apigen`、
  `services/economy`（分页）、`services/session`（两个计数）、`platform`、`clients/minimal`、
  `db/migrations/0005`、`scripts/db-archive.sh`、`deploy/prometheus/hape.rules.yml`、`docs/*`

## 1. 背景

`docs/roadmap.md` §2 列了八件"不被外部条件阻塞、纯代码"的收尾项。契约评审（§1.1 第 3 项）
暂时无法推进，于是先清这八项：它们不影响契约与引擎侧，却能补齐三处**真实缺口**——
权限只到 service 级、`ledger` 读接口无上限、以及三个"该有数字却没有"的计数。

## 2. 目标 / 非目标

目标（八项，逐项有验收）：

1. **op 级 ACL**：凭证可声明 `service.op`，`nakama` 不再能调 `account` 域的所有 op。
2. **越权告警**：`backend_service_scope_denied_total > 0` 持续 10 分钟告警。
3. **分配失败计数**：`backend_allocations_failed_total`，`docs/slo.md` 第 4 条 SLI 才算得出来。
4. **清扫竞态计数**：`backend_session_sweep_races_total`，回答"多久发生一次"。
5. **入站限流**：每类调用方一个令牌桶，超限 429 + `Retry-After`；`apierr.TooManyRequests`
   第一次真正被用上。
6. **读接口分页**：`economy.get_ledger` 支持 `limit` + `after`，响应带 `next_after`。
7. **台账归档**：`scripts/db-archive.sh` 把 N 天前的已结束对局挪进 archive 表；默认 dry-run。
8. **客户端重登**：`clients/minimal` 在 401 时重登一次并重放请求，附自动化断言。

非目标：

- 不做分布式限流（单进程令牌桶；多副本时每副本各自计数，属 M4）。
- 不做归档的自动调度（cron/systemd timer 是部署选择，脚本只保证可重复执行）。
- 不改 UE5 契约、不改任何战的报字段语义。

## 3. 设计与取舍

### 3.1 op 级 ACL（第 1 项）

scope 条目从"service 名"扩展为三种形态：

```text
BACKEND_SERVICE_CREDENTIALS="nakama=<tok>:account.login,account.get_player
                            ,ops=<tok2>:*
                            ,worker=<tok3>:matchmaking+session"
```

- `*`：全放行（必须单独出现，沿用 0016 的规则）。
- `account`：该 service 的所有 op（向后兼容，存量配置不变）。
- `account.login`：**恰好**这一个 op；同一凭证可以混写 service 与 op 条目。

判定：`allowed == "*" || allowed == op.Service || allowed == op.ID`。启动时校验 op 条目
必须命中真实注册的 op（沿用 `UnknownServices` 的思路），拼错即启动失败——否则就是
"静默失权"，正是 0016 里那条教训。

### 3.2 入站限流（第 5 项）

三类调用方各一个令牌桶，键分别是：玩家 id、凭证名、客户端 IP（public op）：

| 类 | 默认 rps / burst | 配置 |
|---|---|---|
| player | 20 / 40 | `BACKEND_RATE_LIMIT_PLAYER`、`..._BURST` |
| service | 200 / 400 | `BACKEND_RATE_LIMIT_SERVICE`、`..._BURST` |
| public | 10 / 20 | `BACKEND_RATE_LIMIT_PUBLIC`、`..._BURST` |

`0` = 关闭这一类（负数在启动时拒绝）。超限返回 429 + `Retry-After: 1`，并计数
`backend_rate_limited_{player,service,public}_total`。

不信任 `X-Forwarded-For`（没有可信代理配置之前，它就是攻击者可控的输入）；
桶按"最后使用时间"惰性清理，避免按 IP 无限增长。

### 3.3 分页（第 6 项）

`GET /v1/economy/players/{id}/ledger?limit=50&after=l_1a2b3c`：

- `limit` 默认 50，上限 200；`after` 是上一页最后一条的 id（游标，不是偏移——账本只追加，
  偏移会重复/漏行）。
- 响应追加 `next_after`；为空表示没有下一页。
- `Op` 增加 `Query []api.QueryParam`，`apigen` 把它渲染成 OpenAPI `in: query` 参数。
  这是契约层的小扩展：此前只有 path 参数。

### 3.4 两个计数（第 3、4 项）

- `backend_allocations_failed_total`：`session.create` 里 `Allocate` 返回错误处 +1
  （无论随后返回 503 还是重试）。
- `backend_session_sweep_races_total`：`create` 发现"分配完成时会话已被清扫关闭"处 +1。

两者在 `platform.New` 里以 0 注册，理由同 0016 的 scope 计数：`== 0` 必须是可查询的事实。

### 3.5 归档（第 7 项）

迁移 `0005_archive.sql` 建 `sessions_archive`、`session_players_archive`、
`match_results_archive`、`match_player_results_archive`（列与源表一致，加 `archived_at`）。

`scripts/db-archive.sh`：

- **必须**给 `ARCHIVE_BEFORE`（缺省拒绝执行——"多久算旧"是业务决定，不给默认值）；
- 默认 dry-run，只打印将搬多少行；`ARCHIVE_CONFIRM=yes` 才写库；
- 单事务：`INSERT INTO ... SELECT ...` + `DELETE ... RETURNING` 按外键顺序（子表先走）；
- 幂等：已归档的行不在源表，重跑是 no-op。

### 3.6 客户端重登（第 8 项）

`clients/minimal` 的请求统一走一个 `session.call`：收到 401 时用同一身份**重登一次**并重放
（只重放一次，避免登录接口故障时打成环）。`main_test.go` 用 `httptest` 断言：
第一次 401 → 重登 → 第二次 200，且重登恰好发生一次。

## 4. 验收

| # | 用例 | 层 |
|---|---|---|
| 1 | `account.login` 之外的 op 被 403，`account.login` 200；混写 service 与 op 条目生效；拼错 op 启动失败 | `pkg/httpx` + `platform` 测试 |
| 2 | 规则文件里存在 `ServiceScopeDenied`，且 promtool 通过、指标漂移测试通过 | `ops` job + `platform` 测试 |
| 3/4 | 分配失败与清扫竞态各 +1，`/metrics` 上可见（0 也注册） | `services/session` + `platform` 测试 |
| 5 | 超过 burst 后 429，`Retry-After` 存在，计数 +1；`0` 时完全不限 | `pkg/httpx` 测试 |
| 6 | `limit`/`after` 翻页无重无漏；上限 200；缺省 50 | `services/economy` 双 store 等价性 + `e2e` |
| 7 | 造一条旧会话 → dry-run 不写 → 确认后行进 archive 表且源表消失 → 重跑 no-op | `ops` job（真 PG） |
| 8 | 401 → 重登 → 重放成功，重登一次 | `clients/minimal` 测试 |

## 5. 回滚

- 1、5：删掉配置即回退（scope 退回 service 级；限流 `0` 关闭）。
- 2、3、4：删规则/指标即可，不影响请求路径。
- 6：`limit`/`after` 是向后兼容的新参数；客户端不传就是旧行为。
- 7：归档表独立，恢复即 `INSERT ... SELECT` 反向搬回；脚本不改源表结构。
- 8：客户端本地回退，服务端无感。

## 6. 未决

- [ ] 限流的默认值要有真实流量再定（现在是"比正常客户端激进得多"的量级）。
- [ ] 归档的保留期、以及 archive 表要不要再降采样，属业务决策。
- [ ] 多副本后限流需要共享计数（M4），现在每副本各算各的。
## 7. 落地记录（实现后补）

| 项 | 落在哪 | 验证 |
|---|---|---|
| 1 op 级 ACL | `pkg/httpx/credentials.go`（`AllowsOp`/`UnknownOps`）、`router.go`、`platform.go` | `credentials_test.go`（解析/拒绝/未知 op）；`platform/auth_test.go::TestOpLevelScopeIsEnforced`（同域兄弟 op 403）；`TestOpScopeTypoFailsAtStartup` |
| 2 越权告警 | `deploy/prometheus/hape.rules.yml`（`ServiceScopeDenied`，同时看强制与观察两个计数） | `ops` job 的 promtool + `platform` 的规则漂移测试 |
| 3 分配失败计数 | `services/session`（`SetMetrics` + `count`），`platform` 装配 | `TestAllocationFailureFailsTheSession` 断言 `backend_allocations_failed_total 1` |
| 4 竞态计数 | 同上 | `TestAllocationRacingTheStaleSweepIsReleased` 断言 `backend_session_sweep_races_total 1` |
| 5 入站限流 | `pkg/httpx/ratelimit.go`、`router.go`、`pkg/config`、`platform` | `ratelimit_test.go`（桶数学/关闭语义/429+Retry-After+计数） |
| 6 分页 | `pkg/api`（`QueryParam`）、`apigen`、`services/economy` 双 store、`api/openapi/economy.json` | `ledger_pagination_test.go`（翻页无重无漏、跨玩家游标 400）；等价性测试走真游标循环 |
| 7 归档 | `db/migrations/0005_archive.sql`、`scripts/db-archive.sh` | `ops` job：缺 `ARCHIVE_BEFORE` 拒绝、dry-run 不写、只搬旧的已释放会话、重跑 no-op |
| 8 客户端重登 | `clients/minimal/main.go`（`caller.do`） | `main_test.go`：401 → 重登一次 → 重放成功；持续 401 不循环；无 `relogin` 时原样返回 |

本机还跑过：`go test ./...`（含 `BACKEND_TEST_DATABASE_URL` 指向本地 PG16，全部 ok）。
## 8. 补记（同日，盘点发现的三件运维缺口）

本文只覆盖了"路线图已命名"的八项。把 A/C/D 之外的东西记进 `docs/roadmap.md` §6 时，
其中最要命的三条顺手做掉了：

| 项 | 落地 | 验证 |
|---|---|---|
| **告警送不到人**（有规则、无 Alertmanager） | `deploy/prometheus/alertmanager.yml`（分组、critical 更快、receivers 故意不带 integration）；prod compose 的 `observability` profile 增加 alertmanager 服务；`prometheus.yml` 加 `alerting` 目标 | `ops` job：`amtool check-config`。**它能证明解析与引用合法，不能证明送达**——文件注释里写明了这一点 |
| **归档只做了一半**（`failed` 不搬、没有反向脚本） | `db-archive.sh` 的谓词改为"没人还需要"：`release_pending = false` 且（已确认释放 或 从未分配）；新增 `scripts/db-archive-restore.sh`（单事务、按外键顺序、id 已活着则拒绝） | `ops` job：failed 的归档/保留各断言一次；恢复后回到源表、离开冷表；重复恢复被拒 |
| **文档门禁缺失**（链接、索引计数、md↔html 镜像） | `scripts/docs-check.sh`；`verify` job 与 `scripts/verify.sh` 都调用 | 本地负例实测：md 多一行 → 抓到；引用不存在的文档 → 抓到 |

这三条都进 `roadmap.md` §6.2/§6.1 并标 ✅，不属于本文原本的八项。
## 9. 补记（同日，工具链与三件工程门禁）

盘点 §6.1 里的三件"便宜的"顺手做掉，同时把工具链统一到 Go 1.27——这两件事互相依赖：
staticcheck/govulncheck 的旧版本在 Go 1.27 下编译不过，而 go.mod 还写着 1.23。

| 项 | 落地 | 验证 |
|---|---|---|
| 工具链统一 | 根 `go.mod` 1.23.0 → **1.27.3**（补丁版本一起钉：1.27.0 的标准库有两个可达 HTTP/2 漏洞，CI 的 `govulncheck` 第一次跑就抓到了）；`Dockerfile` / `Dockerfile.ds` 的 `GO_IMAGE` → `golang:1.27-alpine`；CI 四个 `go-version-file: go.mod` 自动跟随 | CI 全绿；本机 `go1.27.3` 就是实际跑过的版本 |
| staticcheck | CI `verify` job + `scripts/verify.{sh,ps1}`；**版本钉住 v0.8.2** | 首次跑出 5 条真问题（死代码、测试里被丢弃的 status），已全部修掉，不是抑制 |
| govulncheck |同上，钉 v1.9.0；**可达漏洞 = 构建失败** | 首次跑出 3 个可达漏洞，见下 |
| 覆盖率 | `scripts/coverage-check.sh`：不含 `tools/` 的地板 58%（当前 60.5%），CI 强制，本地无数据库时只报告 | 本地负例：`COVERAGE_MIN=99` → 退出 1；告警文案给出实际值 |

### 9.1 依赖升级（govulncheck 逼出来的，不是我挑的）

- `github.com/jackc/pgx/v5` v5.7.6 → **v5.9.2**：`GO-2026-5004`（占位符与 dollar-quoted
  字符串混淆导致 SQL 注入）在 `session.PG.Census` 的路径上**可达**。
- `golang.org/x/text` v0.24.0 → **v0.41.0**：`GO-2026-6629`（precis 解析崩溃）与
  `GO-2026-5970`（非法输入死循环），均在 `pg.Open → pgxpool.NewWithConfig` 路径上可达。

升级后全量测试（含真 Postgres 的等价性套件）绿。ADR 0006 已加补记：钉的版本从 v5.7.6
变为 v5.9.2，理由从"不抬高 Go 下限"变为"修复可达漏洞"。

### 9.2 nakama/modules 不在这次统一里（有意）

插件模块的 `go` 指令跟随的是**官方 pluginbuilder 镜像的工具链**（Nakama 3.22 是 Go 1.22），
不是本仓库的 1.27。写成 1.27 会让 `docker build nakama` 直接失败——ADR 0011 说的"三个版本
必须一起动"就包括这一条。等换 Nakama 版本时一起抬。
## 10. 补记（同日，§6 里四件不需要外部条件的）

| 项 | 落地 | 验证 |
|---|---|---|
| Dependabot | `.github/dependabot.yml`：两个 Go 模块（按周、分组一个 PR）、actions、基础镜像（按月）。`nakama-common` 与 `protobuf` 在插件模块里**显式忽略**——它们必须跟随 Nakama 服务端 tag，自动升级会通过 `go build` 却在 `plugin.Open` 失败（ADR 0011） | 文件被 GitHub 解析（不报错）；无法在 CI 里断言"PR 会来"，这是平台行为 |
| Prometheus 保留 30 天 | prod compose `--storage.tsdb.retention.time=15d → 30d` | `ops` job 的 `promtool check config` 仍通过；与 `docs/slo.md` 的月度窗口一致 |
| Makefile 不再是双份真相 | `scripts/` 被写明为唯一事实来源；CI 每次 `make -n <所有 target>` 解析一遍；`scripts/verify.sh` 有 make 时也跑 | `verify` job 新增一步；解析失败即红 |
| 排行榜 `limit` | `rank.get_leaderboard` 增加 `limit`（1–200，缺省 50，超限截断、非数字 400），与 `get_ledger` 同一套 `httpx.QueryInt` 语义 | 新增 `services/rank/service_test.go`：缺省/指定/上限截断/三种非法值；`api/openapi/rank.json` 重新生成 |

另外把部署手册的保留策略写成了**建议值**（`docs/deploy.md` §11.9）：备份 14 天 + 8 周、归档表永久、
Prometheus 30 天、镜像最近 20 个 sha、日志 20m×5。这些数字要按真实事故复盘调，所以留在手册而不是
硬编码进脚本。