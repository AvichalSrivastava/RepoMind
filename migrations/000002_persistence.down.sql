-- 000002_persistence.down.sql
-- Reverses 000002_persistence.up.sql in dependency order, then
-- recreates the bootstrap marker so 000001 remains the consistent
-- starting state if someone migrates down to that revision.

BEGIN;

DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS analysis;
DROP TABLE IF EXISTS release;
DROP TABLE IF EXISTS pull_request;
DROP TABLE IF EXISTS sprint;
DROP TABLE IF EXISTS repository;

-- Restore the bootstrap marker so 000001's state is consistent.
CREATE TABLE IF NOT EXISTS schema_bootstrap (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    applied_at  timestamptz NOT NULL DEFAULT now(),
    note        text        NOT NULL
);
INSERT INTO schema_bootstrap (note)
VALUES ('repomind schema bootstrap — restored by 000002 down migration');

COMMIT;
