-- 000001_init.up.sql
-- RepoMind bootstrap migration. Full DDL (sprint, repository,
-- pull_request, release, analysis, outbox) is delivered in Module 3
-- (Postgres persistence). This file just proves the migration
-- tooling contract end-to-end.

BEGIN;

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Marker table proving the migration ran. Replaced in Module 3.
CREATE TABLE IF NOT EXISTS schema_bootstrap (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    applied_at  timestamptz NOT NULL DEFAULT now(),
    note        text        NOT NULL
);

INSERT INTO schema_bootstrap (note) VALUES ('repomind schema bootstrap — replaced by Module 3');

COMMIT;
