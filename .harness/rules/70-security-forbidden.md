# 70 - 安全与禁止事项

## 红线：钱与权威

- 货币只走整数 + 账本 + 幂等键；余额永不为负。
- 任何余额变更必须能被同一把幂等键重放而不重复结算。
- 客户端**永不**决定：胜负、分数、余额、价格、MMR、掉落。
- 战斗结果只能来自 Dedicated Server（`AuthService`），客户端伪造必须 401（e2e 已覆盖）。

## 密钥与数据

- 不读取、不输出、不提交任何密钥、token、数据库口令、真实玩家数据。
- 服务 token 只存在于服务端进程环境变量与 CI secret；**绝不**下发给客户端。
- 服务凭证必须声明 **scope**（`name=token:service+service`，`*` 仅限运维工具）：
  最小权限不是可选项。新增 service-auth op 时必须确认 `deploy/systemd/backend.env.example`
  仍能触达它——否则 CI 的覆盖性测试会失败（spec 0016）。
- 日志里不出现：完整 `Authorization` 头、请求体、邮箱、设备 id 原值。
- 示例代码与文档用 `dev-service`、`player-1` 这类假值。

## 环境边界

- 不访问生产环境、不执行生产部署、不连生产数据库。
- 不把仓库代码或数据上传到未知服务。
- 本地只起 `127.0.0.1`；对外暴露需用户明确要求。

## 破坏性命令

禁止：`git reset --hard`、`git clean -fdx`、`git push --force`、`rm -rf`（项目外）、批量删迁移文件。
需要"回到干净状态"时，先 `git stash` 并说明。

## 代码层面的安全约束

- 鉴权只在 `pkg/httpx` 一处实现；handler 不得自定义认证逻辑。
- `RequireSelf` 用于"只能操作自己的资源"；漏掉它就是越权读别人钱包。
- 请求体必须 `DisallowUnknownFields` + 大小上限（1 MiB）。
- 错误响应不泄露内部结构：`code` + 人话 `message` + 白名单 `details`。
- 不引入 `unsafe`、`os/exec` 调外部命令处理用户输入。
- SQL 落地时一律参数化查询；字符串拼接 SQL 视为缺陷。

## 违反处理

1. 立即停止。
2. 说明冲突点（引用本文件对应条目）。
3. 给出合规替代方案。
4. 请求用户确认。
