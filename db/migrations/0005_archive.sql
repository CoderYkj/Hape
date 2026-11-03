-- ============================================================================
-- 0005_archive.sql — 分配台账的归档：把已结束的对局搬到冷表
--
-- 起因（spec 0018 遗留）：sessions 及其子表只增不减，一个长期运营的库会持续变大，
-- 台账查询与告警扫描都会变慢。归档不是"删数据"：行连同战报一起搬进 *_archive，
-- 需要时 INSERT ... SELECT 搬回来即可。
--
-- 只搬"已经确认释放（released_at 非空）且已结束"的会话：仍待回收的会话被
-- reconciler 需要，不能离开源表。门槛由调用方给（ARCHIVE_BEFORE），这里不设默认值。
--
-- 只追加，不改已发布的迁移。
-- ============================================================================

BEGIN;

CREATE TABLE sessions_archive (
    archived_at      timestamptz NOT NULL DEFAULT now(),
    id               text PRIMARY KEY,
    match_id         text        NOT NULL,
    state            text        NOT NULL,
    queue            text        NOT NULL DEFAULT '',
    region           text        NOT NULL DEFAULT '',
    map              text        NOT NULL DEFAULT '',
    allocation_id    text,
    provider         text,
    address          text,
    port             integer,
    result_id        text,
    runtime_token    text,
    created_at       timestamptz NOT NULL,
    ready_at         timestamptz,
    started_at       timestamptz,
    finished_at      timestamptz,
    release_pending  boolean     NOT NULL DEFAULT false,
    release_attempts integer     NOT NULL DEFAULT 0,
    allocated_at     timestamptz,
    released_at      timestamptz
);

COMMENT ON TABLE sessions_archive
    IS '已归档的对局会话（spec 0018 遗留项）；列与 sessions 一致，外加 archived_at';

CREATE TABLE session_players_archive (
    archived_at timestamptz NOT NULL DEFAULT now(),
    session_id  text        NOT NULL,
    player_id   text        NOT NULL,
    PRIMARY KEY (session_id, player_id)
);

CREATE TABLE match_results_archive (
    archived_at      timestamptz NOT NULL DEFAULT now(),
    result_id        text PRIMARY KEY,
    session_id       text        NOT NULL,
    duration_seconds bigint      NOT NULL,
    accepted_at      timestamptz NOT NULL
);

CREATE TABLE match_player_results_archive (
    archived_at timestamptz NOT NULL DEFAULT now(),
    result_id   text    NOT NULL,
    player_id   text    NOT NULL,
    won         boolean NOT NULL,
    score       bigint  NOT NULL,
    kills       bigint  NOT NULL,
    deaths      bigint  NOT NULL,
    damage      bigint  NOT NULL,
    PRIMARY KEY (result_id, player_id)
);

CREATE INDEX session_players_archive_session ON session_players_archive (session_id);
CREATE INDEX match_results_archive_session ON match_results_archive (session_id);
CREATE INDEX sessions_archive_created ON sessions_archive (created_at);

COMMIT;