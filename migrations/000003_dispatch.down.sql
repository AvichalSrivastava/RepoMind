-- 000003_dispatch.down.sql

BEGIN;

DROP INDEX IF EXISTS outbox_dedup_key_uidx;
ALTER TABLE outbox DROP COLUMN IF EXISTS dedup_key;

COMMIT;
