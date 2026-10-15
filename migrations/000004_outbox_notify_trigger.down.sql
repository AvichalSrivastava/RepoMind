-- 000004_outbox_notify_trigger.down.sql

BEGIN;

DROP TRIGGER IF EXISTS outbox_notify_trg ON outbox;
DROP FUNCTION IF EXISTS outbox_notify();

COMMIT;
