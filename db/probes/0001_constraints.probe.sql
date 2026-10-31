-- 0001_constraints.probe.sql — the schema's invariants, asserted against a live
-- database.
--
--   psql -h 127.0.0.1 -p 55432 -U postgres -d gamebackend \
--        -v ON_ERROR_STOP=1 -f db/probes/0001_constraints.probe.sql
--
-- Every probe that *must* be rejected is wrapped in an exception handler, so a
-- silently-accepted bad row aborts the script with a non-zero exit instead of
-- being lost in scrollback. The whole file runs in one transaction and ends with
-- ROLLBACK: it leaves no rows behind and can be re-run at will.
--
-- These are the assertions the Go domain code makes in memory (pkg/apierr +
-- services/*/store.go). If the two ever disagree, one of them is wrong.

BEGIN;

CREATE FUNCTION pg_temp.expect_failure(what text, stmt text) RETURNS void AS $fn$
BEGIN
    BEGIN
        EXECUTE stmt;
    EXCEPTION WHEN others THEN
        RAISE NOTICE 'PASS  rejected: %', what;
        RETURN;
    END;
    RAISE EXCEPTION 'FAIL  % -> the statement was accepted, but the schema must refuse it', what;
END;
$fn$ LANGUAGE plpgsql;

CREATE FUNCTION pg_temp.expect_ok(what text, stmt text) RETURNS void AS $fn$
BEGIN
    EXECUTE stmt;
    RAISE NOTICE 'PASS  accepted: %', what;
END;
$fn$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------- accounts ---
SELECT pg_temp.expect_ok('player insert', $s$
    INSERT INTO players (id, external_id, display_name) VALUES ('p1','ext-1','Alpha'), ('p2','ext-2','Beta')
$s$);

SELECT pg_temp.expect_failure('duplicate external_id (two accounts, one identity)', $s$
    INSERT INTO players (id, external_id, display_name) VALUES ('p3','ext-1','Gamma')
$s$);

SELECT pg_temp.expect_failure('empty display_name', $s$
    INSERT INTO players (id, external_id, display_name) VALUES ('p4','ext-4','')
$s$);

-- ------------------------------------------------------------------- teams ---
SELECT pg_temp.expect_ok('team + owner member', $s$
    INSERT INTO teams (id, name, owner_id) VALUES ('t1','squad','p1');
    INSERT INTO team_members (team_id, player_id, role) VALUES ('t1','p1','owner')
$s$);

SELECT pg_temp.expect_failure('second owner in one team', $s$
    INSERT INTO team_members (team_id, player_id, role) VALUES ('t1','p2','owner')
$s$);

SELECT pg_temp.expect_ok('second player joins as member', $s$
    INSERT INTO team_members (team_id, player_id, role) VALUES ('t1','p2','member')
$s$);

SELECT pg_temp.expect_failure('one player, two teams', $s$
    INSERT INTO teams (id, name, owner_id) VALUES ('t2','squad2','p2');
    INSERT INTO team_members (team_id, player_id, role) VALUES ('t2','p2','owner')
$s$);

-- ----------------------------------------------------------------- economy ---
SELECT pg_temp.expect_ok('wallet starts at zero', $s$
    INSERT INTO wallets (player_id, currency) VALUES ('p1','gold')
$s$);

SELECT pg_temp.expect_failure('negative balance', $s$
    INSERT INTO wallets (player_id, currency, balance) VALUES ('p1','gems',-1)
$s$);

SELECT pg_temp.expect_failure('currency outside the whitelist', $s$
    INSERT INTO wallets (player_id, currency, balance) VALUES ('p1','dogecoin',10)
$s$);

SELECT pg_temp.expect_failure('ledger movement of zero', $s$
    INSERT INTO ledger_entries (player_id, currency, amount, reason, idempotency_key)
    VALUES ('p1','gold',0,'match','k-zero')
$s$);

SELECT pg_temp.expect_ok('ledger movement', $s$
    INSERT INTO ledger_entries (player_id, currency, amount, reason, idempotency_key)
    VALUES ('p1','gold',100,'match','res-1:p1:gold')
$s$);

SELECT pg_temp.expect_failure('the same idempotency key paid twice', $s$
    INSERT INTO ledger_entries (player_id, currency, amount, reason, idempotency_key)
    VALUES ('p1','gold',100,'match','res-1:p1:gold')
$s$);

SELECT pg_temp.expect_ok('the same key is fine for a different player', $s$
    INSERT INTO ledger_entries (player_id, currency, amount, reason, idempotency_key)
    VALUES ('p2','gold',100,'match','res-1:p1:gold')
$s$);

-- ------------------------------------------------------------- matchmaking ---
SELECT pg_temp.expect_ok('ticket is born waiting by default', $s$
    INSERT INTO matchmaking_tickets (id, player_id, queue, mmr) VALUES ('tkt1','p1','ranked_1v1',1000)
$s$);

SELECT pg_temp.expect_failure('two waiting tickets for one player in one queue', $s$
    INSERT INTO matchmaking_tickets (id, player_id, queue, mmr) VALUES ('tkt2','p1','ranked_1v1',1000)
$s$);

SELECT pg_temp.expect_ok('after the first ticket is matched, the player may queue again', $s$
    UPDATE matchmaking_tickets SET status = 'matched', match_id = 'm1' WHERE id = 'tkt1';
    INSERT INTO matchmaking_tickets (id, player_id, queue, mmr) VALUES ('tkt3','p1','ranked_1v1',1000)
$s$);

SELECT pg_temp.expect_failure('a different queue is a different waiting room', $s$
    INSERT INTO matchmaking_tickets (id, player_id, queue, mmr) VALUES ('tkt4','p1','ranked_1v1',1000)
$s$);

-- ---------------------------------------------------------------- sessions ---
SELECT pg_temp.expect_failure('a ready session without an allocation', $s$
    INSERT INTO sessions (id, match_id, state) VALUES ('s1','m1','ready')
$s$);

SELECT pg_temp.expect_ok('an allocating session', $s$
    INSERT INTO sessions (id, match_id, state) VALUES ('s1','m1','allocating')
$s$);

SELECT pg_temp.expect_failure('two sessions for one match', $s$
    INSERT INTO sessions (id, match_id, state) VALUES ('s2','m1','allocating')
$s$);

SELECT pg_temp.expect_failure('finished session without an accepted result', $s$
    UPDATE sessions SET state = 'finished' WHERE id = 's1'
$s$);

SELECT pg_temp.expect_failure('state outside the lifecycle', $s$
    INSERT INTO sessions (id, match_id, state) VALUES ('s3','m3','paused')
$s$);

SELECT pg_temp.expect_ok('ready session once allocated', $s$
    UPDATE sessions SET state = 'ready', allocation_id = 'local-s1-1', provider = 'local',
           address = '10.0.0.7', port = 7777 WHERE id = 's1'
$s$);

-- ------------------------------------------------------------ battle report ---
SELECT pg_temp.expect_ok('first battle report for a session', $s$
    INSERT INTO match_results (result_id, session_id, duration_seconds) VALUES ('res-1','s1',300)
$s$);

SELECT pg_temp.expect_failure('a second battle report for the same session', $s$
    INSERT INTO match_results (result_id, session_id, duration_seconds) VALUES ('res-2','s1',300)
$s$);

SELECT pg_temp.expect_ok('the session finishes with its result attached', $s$
    UPDATE sessions SET state = 'finished', result_id = 'res-1' WHERE id = 's1'
$s$);

SELECT pg_temp.expect_failure('the same result id cannot be reused across sessions', $s$
    INSERT INTO sessions (id, match_id, state, allocation_id, result_id)
    VALUES ('s9','m9','finished','alloc-9','res-1')
$s$);

-- -------------------------------------------------------------------- rank ---
SELECT pg_temp.expect_ok('a rating', $s$
    INSERT INTO player_ratings (board, player_id, rating) VALUES ('ranked_1v1','p1',1000)
$s$);

SELECT pg_temp.expect_failure('a negative rating', $s$
    INSERT INTO player_ratings (board, player_id, rating) VALUES ('ranked_1v1','p2',-5)
$s$);

SELECT pg_temp.expect_ok('an MMR event', $s$
    INSERT INTO rating_events (board, player_id, idempotency_key, delta, rating_after)
    VALUES ('ranked_1v1','p1','res-1:p1:mmr',24,1024)
$s$);

SELECT pg_temp.expect_failure('the same MMR event applied twice', $s$
    INSERT INTO rating_events (board, player_id, idempotency_key, delta, rating_after)
    VALUES ('ranked_1v1','p1','res-1:p1:mmr',24,1024)
$s$);

-- ------------------------------------------------------------------ progress ---
SELECT pg_temp.expect_ok('progress row defaults to level 1, 0 xp', $s$
    INSERT INTO player_progress (player_id) VALUES ('p1')
$s$);

SELECT pg_temp.expect_failure('negative xp', $s$
    INSERT INTO player_progress (player_id, total_xp) VALUES ('p2', -1)
$s$);

SELECT pg_temp.expect_failure('level below 1', $s$
    INSERT INTO player_progress (player_id, level) VALUES ('p2', 0)
$s$);

ROLLBACK;