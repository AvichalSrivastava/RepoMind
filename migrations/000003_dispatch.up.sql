-- 000003_dispatch.up.sql
-- RepoMind Module 9 — webhook + operator-API dispatch enablement.
--
-- Two things, both additive:
--
--   1. outbox.dedup_key — opaque caller-supplied key (the
--      X-GitHub-Delivery header for webhooks, the Idempotency-Key
--      header for operator calls). A UNIQUE index on this column
--      turns at-least-once delivery into effectively-once at the
--      enqueue layer: GitHub retries of the same delivery, or
--      operator retries with the same Idempotency-Key, INSERT once
--      and become no-ops thereafter.
--
--      NULL is allowed (Postgres default: NULLs are distinct in
--      unique indexes), so the M10 ticker can enqueue dedup-less
--      reconcile events without contention.
--
--   2. No schema change is needed for NOTIFY: pg_notify() is a
--      function call the adapter issues inside the same statement /
--      transaction as the INSERT.  Listeners pick up the wake-up
--      on commit. Both the notify channel name and the listener
--      live in the Go code (M10).

BEGIN;

ALTER TABLE outbox
    ADD COLUMN dedup_key TEXT;

-- Full UNIQUE index (not partial). Postgres treats NULL ≠ NULL in
-- unique indexes by default, so the ticker can insert NULL freely.
-- A non-partial index also lets ON CONFLICT (dedup_key) work without
-- an index_predicate clause, keeping the INSERT statements short.
CREATE UNIQUE INDEX outbox_dedup_key_uidx ON outbox (dedup_key);

COMMIT;
