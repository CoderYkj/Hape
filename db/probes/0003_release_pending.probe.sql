-- 0003_release_pending.probe.sql — the release read-back columns, asserted against a
-- live database.
--
--   psql -h 127.0.0.1 -p 55432 -U postgres -d gamebackend \
--        -v ON_ERROR_STOP=1 -f db/probes/0003_release_pending.probe.sql
--
-- Same contract as 0001_constraints: every case that must be rejected is wrapped in an
-- exception handler, the whole file runs in one transaction and ends with ROLLBACK, so
-- it leaves nothing behind and can be re-run at will.

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
    INSERT INTO players (id, external_id, display_name) VALUES ('rp1','ext-rp1','Release')
$s$);

SELECT pg_temp.expect_ok('a new session is not pending release', $s$
    INSERT INTO sessions (id, match_id, state, allocation_id)
    VALUES ('s_rp1','m_rp1','ready','alloc-rp1');
    DO $$
    BEGIN
        IF (SELECT release_pending FROM sessions WHERE id = 's_rp1') THEN
            RAISE EXCEPTION 'a new session defaulted to release_pending = true';
        END IF;
        IF (SELECT release_attempts FROM sessions WHERE id = 's_rp1') <> 0 THEN
            RAISE EXCEPTION 'a new session defaulted to release_attempts <> 0';
        END IF;
    END $$;
$s$);

SELECT pg_temp.expect_ok('marking a released allocation pending is allowed', $s$
    UPDATE sessions SET release_pending = true, release_attempts = 1 WHERE id = 's_rp1'
$s$);

SELECT pg_temp.expect_failure('a pending release without an allocation id', $s$
    UPDATE sessions SET release_pending = true, allocation_id = NULL WHERE id = 's_rp1'
$s$);

SELECT pg_temp.expect_failure('a negative attempt counter', $s$
    UPDATE sessions SET release_attempts = -1 WHERE id = 's_rp1'
$s$);

SELECT pg_temp.expect_ok('clearing the pending flag', $s$
    UPDATE sessions SET release_pending = false WHERE id = 's_rp1'
$s$);

ROLLBACK;