-- 000004_outbox_notify_trigger.up.sql
-- RepoMind Module 9 — wake-up channel for the M10 dispatcher.
--
-- Decision: encode the LISTEN/NOTIFY wake-up as an AFTER INSERT
-- trigger on outbox rather than embedding pg_notify() in the Go
-- INSERT statements. Benefits:
--
--   * Go SQL stays plain INSERT — no Scan(&id, new(any)) wart for
--     pg_notify's void return.
--   * Any inserter (psql, future workers, ops scripts) fires the
--     wake-up automatically. The notification semantics live in
--     one place.
--   * pg_notify queues until COMMIT either way, so behaviour is
--     identical for both pool-direct and tx-wrapped inserts.
--   * 23505 unique_violation (dedup hit) ROLLBACKs the INSERT
--     before the trigger commits, so duplicates correctly do NOT
--     wake the listener.
--
-- The channel name is duplicated in Go as
-- internal/application/persistence/outbox.NotifyChannel. Both must
-- match; changing either requires changing both.

BEGIN;

CREATE OR REPLACE FUNCTION outbox_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    -- Payload is the row's id as text. Listeners parse it back to
    -- a UUID; if parsing fails they fall back to a pull from the
    -- pending queue, so the payload is best-effort, not load-bearing.
    PERFORM pg_notify('repomind_outbox', NEW.id::text);
    RETURN NEW;
END;
$$;

CREATE TRIGGER outbox_notify_trg
    AFTER INSERT ON outbox
    FOR EACH ROW
    EXECUTE FUNCTION outbox_notify();

COMMIT;
