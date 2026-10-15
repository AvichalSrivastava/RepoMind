-- 000005_worker_state.up.sql
-- RepoMind Module 10 — worker-state singleton kv table.
--
-- Purpose: durable bookmarks for the worker subsystems. The sprint
-- ticker uses it to remember "last sprint state I observed" so a
-- restart doesn't re-enqueue release-notes events for unchanged
-- sprints. Future workers (e.g. webhook re-ingest, GitHub backfill)
-- store their own keys here without needing a new table each.
--
-- Schema: tiny key/value with JSONB values + updated_at.
--
--   key         well-known string, owned by a specific subsystem.
--               Convention: "<subsystem>:<scope>" e.g.
--                 "ticker:sprint:<sprint_uuid>"
--                 "webhook_replay:cursor"
--               Free-form so subsystems own their own namespace.
--
--   value       JSONB; subsystems define their own shape. Stored as
--               JSONB (not TEXT) so we can introspect via psql and
--               run jsonb_path_ops queries later if useful.
--
--   updated_at  always set by the adapter to now() on every write,
--               so dashboards can show "last seen X minutes ago".
--
-- The adapter exposes Get / Set only; no Delete (we don't garbage-
-- collect bookmarks — they're tiny and survive forever). No
-- CompareAndSet (single-instance worker; concurrency comes via
-- worker_state being read+written inside the same UoW.Within tx
-- that also enqueues outbox events, so reads/writes are serialised
-- by Postgres SSI/RC isolation).

BEGIN;

CREATE TABLE worker_state (
    key         TEXT        PRIMARY KEY,
    value       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Optional helper index for "show me all ticker bookmarks" type
-- queries. Trigram could help but the key set is small (< 1000
-- expected), so a plain btree on the prefix via text_pattern_ops
-- is plenty.
CREATE INDEX worker_state_key_prefix_idx
    ON worker_state (key text_pattern_ops);

COMMIT;
