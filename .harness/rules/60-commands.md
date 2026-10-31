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
go get <module>            # 违反 stdlib-only 政策，需先写 ADR
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
pwsh -File scripts/verify.ps1

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

失败处理：

1. 读完整错误，判断是编译 / 断言 / 契约漂移 / 格式。
2. 契约漂移 → `go run ./tools/apigen` 后重跑。
3. 同一命令连续失败 2 次 → 停下来汇报，不盲改。

## 环境限制（本机）

- 本机 `GOCACHE` 默认位置可能不可写且工作区磁盘紧张，verify 脚本会把 Go 缓存放到 `%TEMP%`（或 `$TMPDIR`），不写进仓库。
- 仓库在工作树里时，`go build` 可能报 `error obtaining VCS status`，脚本统一带 `-buildvcs=false`。
