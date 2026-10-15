-- 000006_release_content_hash.down.sql

BEGIN;

DROP INDEX IF EXISTS release_sprint_content_hash_idx;
ALTER TABLE release DROP COLUMN IF EXISTS content_hash;

COMMIT;
