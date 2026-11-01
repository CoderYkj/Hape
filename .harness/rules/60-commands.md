# 60 - 命令白名单

## 允许直接执行

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
go test -count=1 ./...
gofmt -l .
gofmt -w .
go run ./cmd/platform
go run ./tools/fakeds
go run ./tools/fakeds -launch-ds exec
go run ./tools/fakeds -launch-ds docker -ds-image hape-ds:ci
go run ./tools/fakeds -launch-ds exec -crash-restart
docker build -f Dockerfile.ds -t hape-ds:ci .
go run ./tools/apigen
go run ./tools/apigen -check
curl -s http://127.0.0.1:8080/healthz
git status
git diff
git diff --stat
git log --oneline -n 20
git switch -c vibe/<slug>
```

## 需说明理由后执行

```bash
go get <module>            # 新依赖需先写 ADR（当前只有 pgx，见 ADR 0006）
go mod tidy                # 可能改动 go.sum
go test -run . -race -count=10 ./...   # 耗时
docker compose -f deploy/docker-compose.yml up
git commit -m "..."        # 用户明确要求时
```

## 禁止（除非用户明确授权）

```bash
任何生产部署：kubectl apply / helm upgrade / terraform apply（生产环境）
git push --force
git reset --hard
git clean -fdx
rm -rf（项目外目录）
读取或打印 .env / 密钥 / 真实玩家数据
curl 生产域名
```

## verify 组合（顺序即依赖）

```bash
# Windows
scripts\verify.cmd              # Windows 5.1；装了 PS7 可用 pwsh -File scripts/verify.ps1

# Linux / CI
make verify
# 等价于：
gofmt -l . | tee /tmp/fmt.txt && test ! -s /tmp/fmt.txt
go vet ./...
go build ./...
go test ./...
go run ./tools/apigen -check
```

> `-race` 需要 cgo，也就是需要 C 编译器（Windows 上通常没有）。缺编译器时
> `scripts/verify.ps1 -Race` 会打印 SKIP 并继续，不要把 SKIP 当成通过；
> CI 的 Linux runner 自带 gcc，那里是真跑的。

## CI

推送 `main` 或开 PR 会跑 `.github/workflows/ci.yml`；它比本机多两件事：
`-race`（需要 cgo，本机没有 C 编译器）和带 Postgres 的等价性测试。
本机 `verify` 绿不代表 CI 绿，反之亦然——两边都跑才算数。

## 战斗服（DS）相关

`ds/` 是参考 Dedicated Server（spec 0012）。它读的是"厂商注入的环境"，
所以单独跑它没有意义——环境得由 `fakeds` 注入：

```bash
go run ./cmd/platform                              # 终端 1
go run ./tools/fakeds -launch-ds exec              # 终端 2：分配 + 启动 ds/ 二进制
go run ./tools/fakeds -launch-ds exec -crash-restart   # 崩溃重启，只结算一次
docker build -f Dockerfile.ds -t hape-ds:ci . && \
  go run ./tools/fakeds -launch-ds docker -ds-image hape-ds:ci
```

判读要点：

- 第一行 banner 必须有 `runtime_token=present(len=N)`，N 与后端发给它的令牌等长；
  **banner 里不该出现 token 值**，出现就是泄漏。
- `applied=true` 只出现一次；`-crash-restart` 的第一次跑退出码是 **17**（刻意崩溃），
  第二次复用同一个 `result_id`。
- 缺 `RUNTIME_TOKEN` 直接退出码 1 并点名该变量——这是设计，不是故障。

## 数据库相关

```bash
# 本机实例（Windows 免安装，见 docs/postgres-local.md）
scripts\pg-local.cmd -Action start|stop|status|migrate|probe|grant

# 跑「内存实现 vs SQL 实现」等价性测试（无此变量则 skip）
$env:BACKEND_TEST_DATABASE_URL = 'postgres://gamebackend:gamebackend@127.0.0.1:55432/gamebackend?sslmode=disable'
go test -count=1 ./services/...         # 七个域的等价性测试（无变量时 skip）

# 整个平台连真库跑一遍
$env:BACKEND_DATABASE_URL = $env:BACKEND_TEST_DATABASE_URL
go run ./cmd/platform   # 另一个终端：go run ./tools/fakeds
```

拉模块在直连 `proxy.golang.org` 不通的网络上需要镜像（`go.sum` 仍记录校验和）：

```bash
go env -w GOPROXY=https://goproxy.cn,direct GOSUMDB=off
```

失败处理：

1. 读完整错误，判断是编译 / 断言 / 契约漂移 / 格式。
2. 契约漂移 → `go run ./tools/apigen` 后重跑。
3. 同一命令连续失败 2 次 → 停下来汇报，不盲改。

## 环境限制（本机）

- 本机 `GOCACHE` 默认位置可能不可写且工作区磁盘紧张，verify 脚本会把 Go 缓存放到 `%TEMP%`（或 `$TMPDIR`），不写进仓库。
- 仓库在工作树里时，`go build` 可能报 `error obtaining VCS status`，脚本统一带 `-buildvcs=false`。
