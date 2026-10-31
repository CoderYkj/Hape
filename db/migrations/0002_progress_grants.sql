-- ============================================================================
-- 0002_progress_grants.sql — 经验入账的幂等承载表
--
-- 起因：0001 的 `player_progress` 只有累计值（total_xp / level / unlocks），
-- 没有地方记录"哪个幂等键已经加过"。内存实现靠进程内 map 去重，进程一重启
-- 就丢——那正好是重放最容易发生的时刻（战斗服重试、平台重启）。
--
-- 只追加，不改 0001：迁移一旦发布就不再编辑。
-- ============================================================================

BEGIN;

CREATE TABLE progress_grants (
    player_id       text        NOT NULL REFERENCES players (id),
    -- 调用方给的幂等键；与 economy 的 ledger_entries 同一约定。
    idempotency_key text        NOT NULL,
    xp              bigint      NOT NULL CHECK (xp > 0),
    reason          text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (player_id, idempotency_key)
);

COMMENT ON TABLE progress_grants IS '经验入账流水 + 幂等去重；重放撞主键即为"已应用"，不再加经验';

COMMIT;