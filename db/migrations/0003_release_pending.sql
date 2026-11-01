-- ============================================================================
-- 0003_release_pending.sql — 战斗服回收的回读与重试
--
-- 起因：`Release` 只发 `DELETE /v1/stop/{id}` 就当成功，而厂商的 200 只表示"接受
-- 了停止请求"。实例可能还在跑，也就还在计费。两个调用点（结算后、超时回收）都是
-- best-effort 只打日志，于是"没释放成功"对系统不可见，只在账单上可见。
--
-- 这两列让回收失败变成一条可重试的记录：reconciler 每一轮重试，次数是唯一的告警信号。
--
-- 只追加，不改 0001/0002：迁移一旦发布就不再编辑。
-- ============================================================================

BEGIN;

ALTER TABLE sessions
    ADD COLUMN release_pending  boolean NOT NULL DEFAULT false,
    ADD COLUMN release_attempts integer NOT NULL DEFAULT 0
        CONSTRAINT sessions_release_attempts_non_negative CHECK (release_attempts >= 0);

COMMENT ON COLUMN sessions.release_pending
    IS '回收尚未被厂商确认（实例可能仍在计费）；reconciler 会重试';
COMMENT ON COLUMN sessions.release_attempts
    IS '回收重试失败次数；同一会话上这个数字持续增长就是漏计费的告警';

-- 待回收必须知道要回收谁。没有 allocation_id 就没有可释放的对象，
-- 置位只能是 bug。
ALTER TABLE sessions
    ADD CONSTRAINT sessions_pending_release_has_allocation
        CHECK (NOT release_pending OR allocation_id IS NOT NULL);

-- 部分索引：清扫只查待回收的那几行，正常情况下这个索引是空的。
CREATE INDEX sessions_release_pending ON sessions (id) WHERE release_pending;

COMMIT;