# ADR 0011 - Nakama 桥接改用 Go 插件（取代 ADR 0009）

- 状态：**accepted**（2026-11-03）
- 取代：ADR 0009（Lua 模块）
- 涉及：`nakama/Dockerfile`（新增）、`nakama/modules/`、`deploy/docker-compose.yml`、CI job `nakama`

## 1. 背景

ADR 0009 当时把桥接从 Go 插件换成 Lua 模块，理由是 Go 插件要在宿主二进制用
`-trimpath` 构建的情况下复现同样的包哈希，实测失败。Lua 跑通了，也进了 CI。

现在明确了产品意图：**桥接要用 Go 实现**（与业务代码同语言、可复用类型与测试、部署形态统一），
Lua 只是过渡方案。所以问题回到原点：**能不能让 Go 插件可靠地加载？**

## 2. 重新诊断当初的失败

原始报错：

```text
plugin.Open("/nakama/data/modules/gamemodule.so"): plugin was built with a different
version of package internal/goarch
```

这是 **Go 1.21+ 的包哈希不匹配**，不是"Go 插件不可用"。哈希由构建输入决定，其中包含
`-trimpath` 设置。宿主 Nakama 二进制由官方镜像在特定工具链下、带 `--trimpath` 构建
（其 Dockerfile 还用 `-asmflags="-trimpath /go/build/nakama"`）；而我们当时是在 CI runner 上
用**任意 Go 工具链、不带 trimpath** 编的 `.so`。两边输入不同 → 同一个 `internal/goarch`
在两边算出的哈希不同 → 加载失败。

结论：**这不是"插件不可以"，而是"插件必须在与宿主一致的构建环境里编"。**

## 3. 决定

1. 用官方 **`heroiclabs/nakama-pluginbuilder:<与运行时完全相同的 tag>`** 镜像构建插件，
   并在其中用 `--trimpath`、`CGO_ENABLED=1`（`nakama/Dockerfile`）。
2. 运行时镜像也用同一 tag（`heroiclabs/nakama:<tag>`），`.so` 直接 COPY 进
   `/nakama/data/modules/`。**构建与运行成对升级**。
3. `nakama/modules/go.mod` 的 `nakama-common` 钉在宿主 release 对应的版本
   （当前 v1.32.0 ↔ Nakama 3.22.0）。
4. **删除 Lua 模块**（`nakama/runtime/login.lua`）与 compose 的挂载；桥接只有一条路径。

## 4. 后果

好处：

- 桥接与业务同语言：错误类型、HTTP 客户端、日志格式、env 读取方式全部一致；
- CI 从"能编译"升级为"**真的被 Nakama 加载并跑通登录**"（`nakama` job 构建镜像 → 起容器 →
  grep `gamemodule loaded` → smoke 探针 → `players` 表有对应行）；
- 不再有两套语义（`nk.http_request` 参数顺序、返回值个数这类差异消失）。

代价（都已接受）：

- **插件加载失败会让 Nakama 起不来**（fail fast）。代价是可用性风险，缓解是 CI 的
  `nakama` job 必须绿；部署检查单里"换 Nakama 版本要同时换 pluginbuilder tag 与
  nakama-common 版本"是硬步骤。
- 多一层构建（pluginbuilder 镜像），CI 时间增加约一个镜像构建。
- 换 Nakama 大版本时，`go.mod`、`Dockerfile` 的 tag、`nakama-common` 版本三处必须一起动。

## 5. 验证

CI `nakama` job：

1. `docker compose up -d --build postgres backend nakama`（`--build` 会编插件镜像）；
2. 断言日志出现 `gamemodule loaded`（Go 插件在 `InitModule` 里打）；
3. `tools/nakama-smoke`：设备登录 → RPC `login` → 后端 `account.login` → 后端读取；
4. 断言 `players` 表有该 `external_id` 的行。

## 6. 回滚

`git` 历史里恢复了 `nakama/runtime/login.lua` 与 compose 的挂载即可回到 Lua 路径
（ADR 0009 作为历史记录保留，不删除）。回滚不需要改任何后端代码：两条路径的 RPC 契约相同。

## 7. 未决

- [ ] 换 Nakama 版本时的自动校验：CI 可以断言 `nakama --version` 与 pluginbuilder tag、
      `nakama-common` 版本三者一致，避免人工漏改。
- [ ] 插件热更新（Nakama 不支持运行时换插件；现在靠重启）。