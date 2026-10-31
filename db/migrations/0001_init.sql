-- ============================================================================
-- 0001_init.sql — 全量初始 schema（Postgres 16）
--
-- 状态：**已在 Postgres 16.4 上执行过**（本机免安装实例，见 docs/postgres-local.md）。
--   约束用 `db/probes/0001_constraints.probe.sql` 逐条验证：幂等键、余额非负、
--   状态机守卫、一人一队、一会话一战报都会被数据库拒绝。
--   仍未接线到 Go：v0 的 7 个服务跑在 `services/*/store.go` 的内存实现上，
--   换 SQL store 是 M1，那一步才需要驱动依赖（见 ADR 0006）。
--
-- 约定：
--   * 时间是 timestamptz，全部用 now() 默认，不接受应用侧本地时间。
--   * 金额/经验/评分一律 bigint（Go 侧 int64），禁止 numeric/float。
--   * 所有业务表的主键是 text（服务生成的不透明 id），不依赖自增。
--   * 幂等靠唯一约束兜底，而不是靠应用层先查后写。
--   * 迁移只追加：已发布的迁移文件永不修改，改 schema 就加 0002_*.sql。
--   * 事务边界：每个请求一个事务；本文件用单个事务包裹，失败整体回滚。
--
-- 与代码的对应：
--   services/account      -> players
--   services/team         -> teams, team_members
--   services/matchmaking  -> matchmaking_tickets
--   services/session      -> sessions, session_players
--   platform/sink.go      -> match_results, match_player_results + 各域写入
--   services/progress     -> player_progress
--   services/economy      -> wallets, ledger_entries
--   services/rank         -> player_ratings, rating_events
-- ============================================================================

BEGIN;

-- ---------------------------------------------------------------------------
-- 账号
-- ---------------------------------------------------------------------------
CREATE TABLE players (
    id           text PRIMARY KEY,
    -- 上游身份（Nakama user_id 或平台渠道 id）。账号的唯一外部锚点。
    external_id  text        NOT NULL UNIQUE,
    display_name text        NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 24),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE  players IS '玩家账号主干；社交/登录态由 Nakama 持有，这里只存后端域需要的投影';
COMMENT ON COLUMN players.external_id IS '外部身份 id，重复注册即 409；account.get-or-create 的幂等键';

-- ---------------------------------------------------------------------------
-- 队伍
-- ---------------------------------------------------------------------------
CREATE TABLE teams (
    id         text PRIMARY KEY,
    name       text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 32),
    owner_id   text        NOT NULL REFERENCES players (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE team_members (
    team_id   text        NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    player_id text        NOT NULL REFERENCES players (id),
    role      text        NOT NULL CHECK (role IN ('owner', 'member')),
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, player_id)
);

-- v0 规则：一个玩家同时只能在一个队伍里（退队才能入新队）。
-- 如果未来要放开多队，删掉这个索引，并把 team.* 的权限判断改成按 team_id 读。
CREATE UNIQUE INDEX team_members_one_team_per_player ON team_members (player_id);

-- 队长必须真的是成员，且角色为 owner。
CREATE UNIQUE INDEX team_members_single_owner ON team_members (team_id) WHERE role = 'owner';

COMMENT ON TABLE team_members IS '队伍成员；owner 行与 teams.owner_id 必须一致（应用侧保证，DB 侧用单 owner 唯一索引兜底）';

-- ---------------------------------------------------------------------------
-- 匹配队列
-- ---------------------------------------------------------------------------
CREATE TABLE matchmaking_tickets (
    id         text PRIMARY KEY,
    player_id  text        NOT NULL REFERENCES players (id),
    queue      text        NOT NULL,
    mmr        bigint      NOT NULL CHECK (mmr >= 0),
    -- 组队匹配时带上整队 id 列表；v0 只有单人票，数组为空。
    party      text[]      NOT NULL DEFAULT '{}',
    -- DEFAULT is not cosmetic: a ticket is born waiting, and every caller that
    -- omits the column would otherwise fail a NOT NULL check.
    status     text        NOT NULL DEFAULT 'waiting' CHECK (status IN ('waiting', 'matched', 'cancelled')),
    match_id   text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- 同一玩家在同一队列只能有一张 waiting 票：重复入队不是新票，而是返回既有票。
CREATE UNIQUE INDEX matchmaking_one_waiting_ticket
    ON matchmaking_tickets (queue, player_id) WHERE status = 'waiting';

-- 撮合扫描的主索引：按队列 + 最早等待优先。
CREATE INDEX matchmaking_waiting_scan
    ON matchmaking_tickets (queue, created_at) WHERE status = 'waiting';

COMMENT ON TABLE matchmaking_tickets IS '票据即状态机的载体：waiting -> matched / cancelled；matched 后必须写 match_id';

-- ---------------------------------------------------------------------------
-- 对局会话
-- ---------------------------------------------------------------------------
CREATE TABLE sessions (
    id            text PRIMARY KEY,
    match_id      text        NOT NULL,
    state         text        NOT NULL CHECK (state IN ('allocating', 'ready', 'in_progress', 'finished', 'failed')),
    queue         text        NOT NULL DEFAULT '',
    region        text        NOT NULL DEFAULT '',
    map           text        NOT NULL DEFAULT '',
    -- 分配器返回的落地信息；state = allocating 时为空。
    allocation_id text,
    provider      text,
    address       text,
    port          integer     CHECK (port IS NULL OR (port > 0 AND port < 65536)),
    -- 战报 id：同一场对局的战报只允许接受一次。
    result_id     text,
    -- 战斗服提供的运行时 token（见 ADR-0003 待定项：v0 只用共享 service token）。
    runtime_token text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    ready_at      timestamptz,
    started_at    timestamptz,
    finished_at   timestamptz,
    CONSTRAINT sessions_ready_has_allocation
        CHECK (state IN ('allocating', 'failed') OR allocation_id IS NOT NULL),
    CONSTRAINT sessions_finished_has_result
        CHECK (state <> 'finished' OR result_id IS NOT NULL)
);

-- 一个 match_id 只能有一场会话；重复 create 是 409，不是新会话。
CREATE UNIQUE INDEX sessions_one_per_match ON sessions (match_id);

-- 战报不可重复挂载：result_id 全局唯一，天然挡住同一场战报的二次提交。
CREATE UNIQUE INDEX sessions_result_id_unique ON sessions (result_id) WHERE result_id IS NOT NULL;

-- 运维排查：卡在 allocating 的会话（僵尸分配）需要按时间扫。
CREATE INDEX sessions_state_created ON sessions (state, created_at);

COMMENT ON TABLE sessions IS '对局会话状态机 allocating -> ready -> in_progress -> finished（+ failed）；状态边只由 session.CanTransition 定义';

CREATE TABLE session_players (
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    player_id  text NOT NULL,
    PRIMARY KEY (session_id, player_id)
);

COMMENT ON TABLE session_players IS '本场参战名单；结算时用它校验战报里的玩家集合是否一致';

-- ---------------------------------------------------------------------------
-- 战报（战斗服唯一上报入口的落库形态）
-- ---------------------------------------------------------------------------
CREATE TABLE match_results (
    result_id        text PRIMARY KEY,
    session_id       text        NOT NULL UNIQUE REFERENCES sessions (id),
    duration_seconds bigint      NOT NULL CHECK (duration_seconds >= 0),
    accepted_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE match_player_results (
    result_id text   NOT NULL REFERENCES match_results (result_id) ON DELETE CASCADE,
    player_id text   NOT NULL,
    won       boolean NOT NULL,
    score     bigint  NOT NULL CHECK (score >= 0),
    kills     bigint  NOT NULL CHECK (kills >= 0),
    deaths    bigint  NOT NULL CHECK (deaths >= 0),
    damage    bigint  NOT NULL CHECK (damage >= 0),
    PRIMARY KEY (result_id, player_id)
);

COMMENT ON TABLE match_results IS '战斗服上报的战报头；result_id 幂等键 = 会话内唯一，重复上报返回既有结果';

-- ---------------------------------------------------------------------------
-- 进度
-- ---------------------------------------------------------------------------
CREATE TABLE player_progress (
    player_id  text PRIMARY KEY REFERENCES players (id),
    total_xp   bigint      NOT NULL DEFAULT 0 CHECK (total_xp >= 0),
    level      integer     NOT NULL DEFAULT 1 CHECK (level >= 1),
    unlocks    text[]      NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE player_progress IS 'XP 单调累加（永不减少）；level 由 XPPerLevel 派生，写入时由服务计算';

-- ---------------------------------------------------------------------------
-- 经济
-- ---------------------------------------------------------------------------
CREATE TABLE wallets (
    player_id  text        NOT NULL REFERENCES players (id),
    currency   text        NOT NULL CHECK (currency IN ('gold', 'gems')),
    balance    bigint      NOT NULL DEFAULT 0 CHECK (balance >= 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (player_id, currency)
);

CREATE TABLE ledger_entries (
    id              bigserial PRIMARY KEY,
    player_id       text        NOT NULL REFERENCES players (id),
    currency        text        NOT NULL CHECK (currency IN ('gold', 'gems')),
    -- 正数入账、负数出账，0 无意义。
    amount          bigint      NOT NULL CHECK (amount <> 0),
    reason          text        NOT NULL,
    -- 幂等键：header Idempotency-Key，或结算时 resultID:playerID:currency。
    idempotency_key text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- 同一玩家同一幂等键只能有一条流水：重放请求命中唯一冲突 -> 返回原结果，不加钱。
CREATE UNIQUE INDEX ledger_entries_idempotent
    ON ledger_entries (player_id, idempotency_key);

CREATE INDEX ledger_entries_player_time ON ledger_entries (player_id, created_at DESC);

COMMENT ON TABLE ledger_entries IS '只追加的流水账；wallet.balance 是它的物化视图，二者必须在同一事务内写';

-- ---------------------------------------------------------------------------
-- 排行
-- ---------------------------------------------------------------------------
CREATE TABLE player_ratings (
    board      text        NOT NULL,
    player_id  text        NOT NULL REFERENCES players (id),
    rating     bigint      NOT NULL CHECK (rating >= 0),
    wins       bigint      NOT NULL DEFAULT 0 CHECK (wins >= 0),
    losses     bigint      NOT NULL DEFAULT 0 CHECK (losses >= 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (board, player_id)
);

-- 排行榜读取路径：board 内按 rating 降序取前 N。
CREATE INDEX player_ratings_leaderboard ON player_ratings (board, rating DESC);

CREATE TABLE rating_events (
    board           text        NOT NULL,
    player_id       text        NOT NULL REFERENCES players (id),
    idempotency_key text        NOT NULL,
    delta           bigint      NOT NULL,
    rating_after    bigint      NOT NULL CHECK (rating_after >= 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (board, player_id, idempotency_key)
);

COMMENT ON TABLE rating_events IS 'MMR 变更流水 + 幂等去重；重复结算同一位玩家的同一局直接跳过';

COMMIT;