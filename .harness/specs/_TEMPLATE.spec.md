# <编号> - <标题>

- 状态：draft | accepted | implemented | superseded
- 日期：YYYY-MM-DD
- 关联 ADR：<编号或"无">
- 涉及服务：account | team | matchmaking | session | progress | economy | rank | battle

## 1. 背景

为什么现在要做这件事。当前实现有什么缺口，谁受影响（玩家 / 运营 / 客服）。

## 2. 目标 / 非目标

- 目标（可验证，3 条以内）：
- 非目标（明确划出边界，避免范围蔓延）：

## 3. 用户故事与验收标准

用 Given / When / Then 写，一条一个场景。

```text
Given 两名玩家在 ranked_1v1 排队
When  matchmaker 执行一次 pop
Then  生成一个 match_id，两张 ticket 变为 matched
```

## 4. 契约变更

| op id | 方法 | 路径 | 认证 | 幂等 | 说明 |
|---|---|---|---|---|---|
| `session.submit_result` | POST | `/v1/session/sessions/{session_id}/results` | service | 是 | 新增 |

破坏性变更需注明迁移方案与保留期。

## 5. 数据模型变更

字段级说明；是否需要迁移；回填策略。

## 6. 不变量与失败模式

- 必须永远成立的不变量（如"余额不为负"）：
- 失败模式与对应响应码：

## 7. 可观测性

新增日志字段、指标、告警。

## 8. 测试计划

| 层 | 用例 |
|---|---|
| 单元 | |
| 契约 | |
| e2e | |

## 9. 回滚方案

能否只回滚代码；数据是否需要反向迁移；契约是否需要保留旧路径。

## 10. 未决问题

- [ ] 问题 / 谁来定
