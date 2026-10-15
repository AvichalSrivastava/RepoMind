-- 000007: sprint -> confluence page mapping
--
-- One row per sprint that has ever been published to a per-sprint
-- Confluence page. The mapping is authoritative: on republish the
-- handler updates the existing page rather than creating a duplicate.
--
-- We do NOT foreign-key into sprint(id) because sprints may be
-- flushed / re-mirrored (see the M11 backfill flow) while their
-- Confluence pages persist. A missing sprint row is not a reason to
-- delete a live Confluence page.
CREATE TABLE IF NOT EXISTS sprint_confluence_page (
    sprint_id   UUID PRIMARY KEY,
    page_id     TEXT NOT NULL,
    page_title  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Two sprints must not accidentally collide on the same page. This
-- catches the classic "title formatter regression" that silently
-- overwrites another sprint's page.
CREATE UNIQUE INDEX IF NOT EXISTS ux_sprint_confluence_page_page_id
    ON sprint_confluence_page (page_id);
