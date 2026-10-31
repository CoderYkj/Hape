# 本机 Postgres（免安装）

这页记录的是**这台开发机实际装了什么、怎么起停、怎么验证**，不是生产部署文档
（生产见 `docs/deploy.md`）。

## 为什么是这样装的

| 约束 | 后果 |
|---|---|
| 没有管理员权限（UAC 弹不出来） | EnterpriseDB 的 MSI 安装包装不了（要写服务、要提权） |
| `D:` 只剩 ~0.4 GB | 不能把 400 MB 的 Postgres 放进仓库目录 |
| `C:` 有 ~7 GB | 装到 `C:\Users\CoderYkj\pgsql16`，纯用户态 |

于是走**绿色免安装**：下 EnterpriseDB 的 binaries zip，解压，用自己的账号
`initdb`，用自己的进程跑 `postgres.exe`。不装服务、不写注册表、不碰系统目录。
代价是没有开机自启、没有服务托管、没有密码策略——这是开发库，不是数据库方案。

## 现状

| 项 | 值 |
|---|---|
| 版本 | PostgreSQL 16.4（Windows x64，Visual C++ build 1940） |
| 二进制 | `C:\Users\CoderYkj\pgsql16\pgsql\bin` |
| 数据目录 | `C:\Users\CoderYkj\pgsql16\data` |
| 监听 | `127.0.0.1:55432`（**不是 5432**，避免和真库撞端口） |
| 认证 | `trust`（仅 loopback） |
| 超级用户 | `postgres`（无密码） |
| 应用角色/库 | `gamebackend` / `gamebackend` |
| DSN | `postgres://gamebackend:gamebackend@127.0.0.1:55432/gamebackend?sslmode=disable` |
| 日志 | `C:\Users\CoderYkj\pgsql16\server.log` |

## 启停

全部通过 `scripts/pg-local.ps1`（`-PgHome` / `-Port` 可覆盖默认值）：

```powershell
pwsh -File scripts/pg-local.ps1 -Action init      # 首次：下载 + 解压 + initdb（已做过）
pwsh -File scripts/pg-local.ps1 -Action start     # 启动（Start-Process，不占终端）
pwsh -File scripts/pg-local.ps1 -Action status    # pg_isready + select version()
pwsh -File scripts/pg-local.ps1 -Action stop      # pg_ctl -m fast stop
pwsh -File scripts/pg-local.ps1 -Action psql      # 交互式 psql
```

线上/CI 之外的 Linux 机器上不用这个脚本，直接 `initdb` + `pg_ctl`；命令等价。

## 迁移与验证

```powershell
pwsh -File scripts/pg-local.ps1 -Action migrate   # 应用 db/migrations/*.sql（幂等，跳过已应用的）
pwsh -File scripts/pg-local.ps1 -Action probe     # 跑 db/probes/*.probe.sql
```

`migrate` 用一张 `schema_migrations(version)` 表记录已应用文件。它**不是**迁移框架：
应用文件和记录版本是两条语句，不在一个事务里。生产环境要么用成熟工具，要么把
"应用 + 记账"包进一个事务（见 `docs/deploy.md` 第 7 节）。

`probe` 是本仓库对 schema 的断言集：35 条，每条要么"必须被接受"、要么"必须被拒绝"，
被接受的不该被接受的直接让脚本非零退出。它证明的是**数据库层**的不变量，和 Go 侧
`pkg/apierr` + `services/*/store.go` 的内存断言一一对应。

## 已验证（本机实跑）

- `db/migrations/0001_init.sql` 应用成功，13 张表
- `db/probes/0001_constraints.probe.sql`：**35 PASS / 0 FAIL**
- 重复 `migrate` 正确跳过（幂等）
- 修掉的真实缺陷：`matchmaking_tickets.status` 缺 `DEFAULT 'waiting'`，
  没有它任何"建排队票"的写法都会被 NOT NULL 拦下

## 未验证 / 未做

- Go 侧没有任何一行连数据库：7 个服务仍跑 `services/*/store.go` 的内存实现。
  接 SQL store 是 M1，需要先定驱动（见 ADR 0006）。
- 没有 CI：CI 里没有 Postgres 服务，`probe` 目前只能手工跑。要进 CI 得给
  workflow 加 `services: postgres`，那一步没验证过。
- 没有备份、没有复制、没有连接池调优、没有密码策略。

## 卸载

```powershell
pwsh -File scripts/pg-local.ps1 -Action stop
Remove-Item -Recurse -Force C:\Users\CoderYkj\pgsql16
```

没有任何系统级残留（不写注册表、不装服务、不加 PATH）。