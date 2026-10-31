# 50 - Git

## 分支

- `vibe/<slug>`（Agent 专用）
- `feat/<slug>`、`fix/<slug>`、`chore/<slug>`
- 从 `main` 拉出，PR 回 `main`。

## 提交信息（Conventional Commits，scope = 服务名或包名）

```text
feat(session): accept authoritative battle report
fix(economy): reject negative grant amounts
test(e2e): cover duplicate battle report replay
chore(ci): pin go version
refactor(platform): move reward rules into matchSink
```

## 规则

- 一个提交只做一件事；契约变更与实现变更可以同提交，但必须在消息里点明。
- 提交前必须 `verify` 全绿，且 `api/openapi/*` 已随契约一起更新。
- 不提交：`.env`、密钥、`api/openapi` 之外的生成物、二进制、`.tools/`、日志。
- 不 `--no-verify`。
- 不 `git push --force` 到共享分支。

## Agent 默认行为

- **默认不 commit、不 push、不打 tag、不部署**。
- 用户明确要求时才执行；执行前展示 `git status` 与 `git diff --stat`。
- 不做 `git reset --hard` / `git clean -fdx`（见 `70-security-forbidden.md`）。
