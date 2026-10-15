-- 000006_release_content_hash.up.sql
-- RepoMind Module 10 Phase 5 — release-notes idempotency / dedup.
--
-- Adds `content_hash` to `release`. The release-notes use case
-- computes a SHA-256 over (sprint_id + sorted PR ids + merge SHAs)
-- and stores it on every draft. On re-trigger, the handler queries
-- the latest release for the sprint and, if the new hash matches AND
-- the prior release published recently, SKIPS the entire pipeline:
-- no LLM call, no Confluence write, no new row.
--
-- This is layer L3 of the duplicate-prevention strategy documented
-- in docs/ARCHITECTURE.md §6. Layers L4 (UNIQUE(sprint_id, version)
-- on this table) and L5/L6 (full-page Confluence PUT + 409 retry)
-- already exist; this column powers L3.
--
-- NULLs are allowed -- existing rows from before this migration have
-- no hash, and that's fine: the handler treats NULL as "unknown,
-- recompute". After every dispatcher run the hash is populated for
-- subsequent releases.
--
-- The partial index keeps the (sprint_id, content_hash) lookup fast
-- without indexing the historical NULLs.

BEGIN;

ALTER TABLE release ADD COLUMN content_hash TEXT;

CREATE INDEX release_sprint_content_hash_idx
    ON release (sprint_id, content_hash)
    WHERE content_hash IS NOT NULL;

COMMIT;
