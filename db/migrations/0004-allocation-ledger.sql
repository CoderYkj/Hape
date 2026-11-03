-- ============================================================================
-- 0004-allocation-ledger.sql — 分配台账：何时拿到实例、何时确认它没了
--
-- 起因：M2 的"计费对账"缺的不是账单接口，而是**我方的时间线**。会话表记了
-- allocation_id，但没记"什么时候拿到的"和"什么时候确认释放的"，于是：
--   - 无法回答"这台实例被确认释放了吗、跑了多少秒"；
--   - 无法算每场对局的实例成本；
--   - 无法区分"刚分配还没来得及释放"与"分配很久了却没释放"（后者才是漏计费）。
--
-- 只追加，不改已发布的迁移。
-- ============================================================================

BEGIN;

ALTER TABLE sessions
    ADD COLUMN allocated_at timestamptz,
    ADD COLUMN released_at  timestamptz;

COMMENT ON COLUMN sessions.allocated_at
    IS '分配器返回实例的时刻；尚未分配时为空';
COMMENT ON COLUMN sessions.released_at
    IS '厂商回读确认实例已停止的时刻；未确认（或仍在重试）时为空';

-- 释放一定发生在分配之后，且没有分配就不可能有释放。
ALTER TABLE sessions
    ADD CONSTRAINT sessions_released_after_allocation
        CHECK (released_at IS NULL
               OR (allocated_at IS NOT NULL AND released_at >= allocated_at));

-- 部分索引：台账/告警只关心"有分配但没确认释放"的那几行，正常情况下它是空的。
CREATE INDEX sessions_unreleased ON sessions (allocated_at)
    WHERE allocated_at IS NOT NULL AND released_at IS NULL;

COMMIT;