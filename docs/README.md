# 文档索引

> 一个入口：这里的文档怎么分工、哪份是活文档、哪份是历史记录。

## 1. 按"你想回答什么问题"找

| 你的问题 | 读这份 | 类型 |
|---|---|---|
| 现在长什么样？每个环节做什么、验证到哪一步？ | `architecture-map.html`（可视化）/ `architecture-map.md`（源） | 活文档 |
| 设计细节：数据流、包边界、存储选择、失败语义 | `architecture.md` | 活文档 |
| 还剩什么没做、下一步做什么 | `roadmap.html` / `roadmap.md` | 活文档 |
| 怎么在本机跑起来 | `runbook-local-dev.md` | 活文档 |
| 本机起数据库、验证 schema | `postgres-local.md` | 活文档 |
| 怎么部署：镜像、编排、迁移、备份恢复、回滚 | `deploy.md` | 活文档 |
| 上线后怎么看、怎么查：指标、告警、台账、压测 | `observability.md` | 活文档 |
| 我们要承诺什么性能与可用性 | `slo.md` | **草案**（待业务确认） |
| 接口怎么定义、怎么防漂移 | `api-contract.md` | 活文档 |
| 战斗服要遵守什么契约（评审 / 签署用） | `ds-runtime-contract.md` | **契约 v1（评审稿）** |
| 怎么实现一台 UE5 DS | `ue5-ds-integration.md` | 活文档（实现指南） |
| 客户端那半边做什么 | `ue5-client-model.md` | 活文档 |
| 第一家托管厂商怎么接 | `edgegap-setup.md` | 活文档 |
| 给非技术干系人讲这套东西 | `stakeholder-briefing.md` | 活文档（宣讲材料） |
| 为什么当初这么定 | `.harness/specs/`（19 份）+ `.harness/adr/`（10 份） | **记录**（只追加） |
| 工程规则：怎么提交、怎么测、契约怎么改 | `.harness/rules/`（11 份） | 规则 |

## 2. 四类文档，规则不同

| 类型 | 例子 | 能不能改 |
|---|---|---|
| **活文档** | `architecture-map`、`roadmap`、`deploy`、`observability`、`runbook-local-dev`、`api-contract`、`ue5-*`、`edgegap-setup` | 随代码改，且**在同一次提交里改** |
| **可视化镜像** | `*.html` | 由同名 `.md` 同步；不一致时**以 md 为准**（`architecture-map.md` ↔ `architecture-map.html`、`roadmap.md` ↔ `roadmap.html`） |
| **历史记录** | `.harness/specs/`、`.harness/adr/` | **只追加**。要推翻旧的，写新的一份并标注 supersedes |
| **草案 / 评审稿** | `slo.md`（草案 v0）、`ds-runtime-contract.md`（契约 v1） | 拍板/签署后去掉"草案/稿"字样，并同步代码与验收清单 |

## 3. 约定

- 每份活文档第一行抬头写清：**读者 · 状态 · 最后更新 · 上游 spec/ADR**。
- `md` 是源，`html` 是镜像；**同名成对**，改一份就要同步另一份。
- 所有链接用相对路径。仓库里没有链接检查器，所以**改文件名必须 grep 全仓**——
  这条是踩出来的：`architecture.html` 曾被引用成 `architecture-map.html`，6 处链接同时失效。
- 未验证的事写进文档，不靠口头约定；"已验证"必须能指到 CI job 或一次实跑记录。
- 编号连续：spec `0001`–`0019`；ADR `0001`–`0010`（没有 `0008`）。

## 4. 三条推荐阅读路径

| 场景 | 顺序 |
|---|---|
| 新人上手 | `README.md` → 本页 → `architecture-map.md` → `runbook-local-dev.md` → `api-contract.md` |
| 准备交付 | `roadmap.md` → `ds-runtime-contract.md`（交给引擎组）→ `deploy.md` → `observability.md` → `slo.md` |
| 宣讲 / 面试 | `stakeholder-briefing.md` → `architecture-map.html` → 终端跑一局（`tools/fakeds`） |

## 5. 维护这页

新增、改名、拆分文档时，改三处并跑一次链接检查（下面这条命令）：

```bash
# 列出所有 docs/ 引用并检查目标是否存在
grep -rho 'docs/[a-z0-9-]*\.\(md\|html\)' --include='*.md' --include='*.html' . | sort -u |
  while read -r p; do [ -e "$p" ] || echo "MISSING $p"; done
```