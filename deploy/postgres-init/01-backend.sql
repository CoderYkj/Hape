-- 01-backend.sql — create what the Go backend needs, once, on first boot.
--
-- Mounted at /docker-entrypoint-initdb.d/, so the official postgres image runs it
-- only when the data directory is empty. On an existing volume it does NOT run:
-- reset with `docker compose down -v`, or create the role and database by hand.
--
-- The nakama database/user come from POSTGRES_DB/POSTGRES_USER in compose. The
-- backend gets its own database on purpose: two applications sharing one schema is
-- how a migration from one of them breaks the other.

CREATE ROLE gamebackend LOGIN PASSWORD 'gamebackend';
CREATE DATABASE gamebackend OWNER gamebackend;

-- The backend applies migrations as this role (see the CI compose job), so it
-- needs nothing beyond ownership of its own database.