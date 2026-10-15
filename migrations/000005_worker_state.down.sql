-- 000005_worker_state.down.sql

BEGIN;

DROP INDEX IF EXISTS worker_state_key_prefix_idx;
DROP TABLE  IF EXISTS worker_state;

COMMIT;
