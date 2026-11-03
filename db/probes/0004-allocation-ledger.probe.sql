-- 0004-allocation-ledger.probe.sql — 分配台账两列的约束，对着活库断言。
--
--   psql -h 127.0.0.1 -p 55432 -U postgres -d gamebackend \
--        -v ON_ERROR_STOP=1 -f db/probes/0004-allocation-ledger.probe.sql
--
-- 与 0001/0003 同一约定：必须被拒的用例包在异常处理里，整个文件一个事务、最后 ROLLBACK，
-- 不留数据、可反复执行。

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

SELECT pg_temp.expect_ok('fixture player', $s$
    INSERT INTO players (id, external_id, display_name) VALUES ('al1','ext-al1','Ledger')
$s$);

SELECT pg_temp.expect_ok('a session starts without an allocation', $s$
    INSERT INTO sessions (id, match_id, state) VALUES ('s_al1','m_al1','allocating')
$s$);

SELECT pg_temp.expect_ok('recording when the allocation landed', $s$
    UPDATE sessions SET allocated_at = now() WHERE id = 's_al1'
$s$);

SELECT pg_temp.expect_ok('a release after the allocation', $s$
    UPDATE sessions SET released_at = allocated_at + interval '3 seconds' WHERE id = 's_al1'
$s$);

SELECT pg_temp.expect_failure('a release before the allocation', $s$
    UPDATE sessions SET released_at = allocated_at - interval '1 second' WHERE id = 's_al1'
$s$);

SELECT pg_temp.expect_failure('a release with no allocation time at all', $s$
    INSERT INTO sessions (id, match_id, state, released_at)
    VALUES ('s_al2','m_al2','failed', now())
$s$);

SELECT pg_temp.expect_failure('a release on a session that never allocated', $s$
    INSERT INTO sessions (id, match_id, state) VALUES ('s_al3','m_al3','failed');
    UPDATE sessions SET released_at = now() WHERE id = 's_al3'
$s$);

ROLLBACK;