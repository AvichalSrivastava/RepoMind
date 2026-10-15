-- 000002_persistence.up.sql
-- RepoMind Module 3 — persistence schema.
--
-- Drops the bootstrap marker from 000001 and creates the real tables.
-- Tables (in dependency order):
--   repository       — tracked git repositories
--   sprint           — Jira-mirrored sprints (one team, one board)
--   pull_request     — GitHub PRs, optionally cross-linked to sprint via tickets[]
--   release          — versioned release-notes bundle (sprint × version)
--   analysis         — LLM output rows (kind-discriminated, exactly one subject)
--   outbox           — transactional outbox for reliable side-effect dispatch
--
-- Conventions:
--   * UUIDs everywhere; gen_random_uuid() from pgcrypto.
--   * timestamptz only; never plain timestamp.
--   * jsonb (not json) for nested value objects to enable indexing.
--   * CHECK enums on text columns rather than SQL enum types so we can
--     add values without ALTER TYPE … RENAME dances. Domain enforces
--     vocabulary anyway.
--   * Foreign keys are ON DELETE CASCADE for parent-owned children
--     (analysis -> pr/sprint/release; pull_request -> repository) and
--     ON DELETE RESTRICT for cross-aggregate refs that must not orphan
--     (release -> sprint).

BEGIN;

DROP TABLE IF EXISTS schema_bootstrap;

-- ---------- repository ----------

CREATE TABLE repository (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner               text        NOT NULL CHECK (length(owner) > 0),
    name                text        NOT NULL CHECK (length(name)  > 0),
    default_branch      text        NOT NULL,
    qa_branch           text        NOT NULL,
    uat_branch          text        NOT NULL,
    production_branch   text        NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    last_observed_at    timestamptz,
    CONSTRAINT repository_full_name_unique UNIQUE (owner, name)
);

-- ---------- sprint ----------

CREATE TABLE sprint (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    jira_sprint_id      bigint      NOT NULL,
    board_id            bigint      NOT NULL,
    team                text        NOT NULL CHECK (length(team) > 0),
    name                text        NOT NULL CHECK (length(name) > 0),
    state               text        NOT NULL CHECK (state IN ('future','active','closed')),
    goal                text        NOT NULL DEFAULT '',
    starts_at           timestamptz,
    ends_at             timestamptz,
    completed_at        timestamptz,
    mirrored_at         timestamptz NOT NULL,
    CONSTRAINT sprint_jira_id_unique UNIQUE (jira_sprint_id),
    -- Closed iff completed_at is set.
    CONSTRAINT sprint_completed_consistency CHECK (
        (state = 'closed' AND completed_at IS NOT NULL) OR
        (state <> 'closed' AND completed_at IS NULL)
    )
);

CREATE INDEX sprint_team_state_idx ON sprint (team, state);

-- ---------- pull_request ----------

CREATE TABLE pull_request (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id       uuid        NOT NULL REFERENCES repository(id) ON DELETE CASCADE,
    number              integer     NOT NULL CHECK (number > 0),
    title               text        NOT NULL,
    description         text        NOT NULL DEFAULT '',
    author_login        text        NOT NULL,
    author_email        text        NOT NULL DEFAULT '',
    source_branch       text        NOT NULL,
    target_branch       text        NOT NULL,
    state               text        NOT NULL CHECK (state IN ('open','merged','closed')),
    labels              jsonb       NOT NULL DEFAULT '[]'::jsonb,
    tickets             jsonb       NOT NULL DEFAULT '[]'::jsonb,
    files               jsonb       NOT NULL DEFAULT '[]'::jsonb,
    commits             jsonb       NOT NULL DEFAULT '[]'::jsonb,
    reviewers           jsonb       NOT NULL DEFAULT '[]'::jsonb,
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    merged_at           timestamptz,
    closed_at           timestamptz,
    merge_sha           text,
    CONSTRAINT pr_repo_number_unique UNIQUE (repository_id, number),
    -- Merged state must carry merged_at + merge_sha; non-merged must not.
    CONSTRAINT pr_merged_consistency CHECK (
        (state = 'merged' AND merged_at IS NOT NULL AND merge_sha IS NOT NULL) OR
        (state <> 'merged' AND merge_sha IS NULL)
    )
);

CREATE INDEX pr_state_idx      ON pull_request (state);
CREATE INDEX pr_merged_at_idx  ON pull_request (merged_at) WHERE merged_at IS NOT NULL;
CREATE INDEX pr_tickets_gin    ON pull_request USING gin (tickets jsonb_path_ops);

-- ---------- release ----------

CREATE TABLE release (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    sprint_id           uuid        NOT NULL REFERENCES sprint(id) ON DELETE RESTRICT,
    version             text        NOT NULL,
    status              text        NOT NULL CHECK (status IN ('draft','published')),
    notes               text        NOT NULL DEFAULT '',
    pull_request_ids    jsonb       NOT NULL DEFAULT '[]'::jsonb,
    created_at          timestamptz NOT NULL,
    published_at        timestamptz,
    CONSTRAINT release_sprint_version_unique UNIQUE (sprint_id, version),
    CONSTRAINT release_published_consistency CHECK (
        (status = 'published' AND published_at IS NOT NULL) OR
        (status <> 'published' AND published_at IS NULL)
    )
);

CREATE INDEX release_sprint_idx ON release (sprint_id);
CREATE INDEX release_status_idx ON release (status);

-- ---------- analysis ----------

CREATE TABLE analysis (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind                text        NOT NULL CHECK (kind IN ('risk','release_notes','qa_recommendations')),
    subject_pr_id       uuid        REFERENCES pull_request(id) ON DELETE CASCADE,
    subject_sprint_id   uuid        REFERENCES sprint(id)       ON DELETE CASCADE,
    subject_release_id  uuid        REFERENCES release(id)      ON DELETE CASCADE,
    analyzer_name       text        NOT NULL CHECK (length(analyzer_name) > 0),
    analyzer_version    text        NOT NULL DEFAULT '',
    prompt_version      text        NOT NULL DEFAULT '',
    payload             jsonb       NOT NULL,
    model               text        NOT NULL DEFAULT '',
    tokens_used         integer     NOT NULL DEFAULT 0 CHECK (tokens_used >= 0),
    generated_at        timestamptz NOT NULL,
    -- Exactly one subject column may be set (mirrors analysis.Subject in domain).
    CONSTRAINT analysis_one_subject CHECK (
        (CASE WHEN subject_pr_id      IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN subject_sprint_id  IS NOT NULL THEN 1 ELSE 0 END) +
        (CASE WHEN subject_release_id IS NOT NULL THEN 1 ELSE 0 END) = 1
    )
);

CREATE INDEX analysis_pr_kind_idx      ON analysis (subject_pr_id, kind)      WHERE subject_pr_id      IS NOT NULL;
CREATE INDEX analysis_sprint_kind_idx  ON analysis (subject_sprint_id, kind)  WHERE subject_sprint_id  IS NOT NULL;
CREATE INDEX analysis_release_kind_idx ON analysis (subject_release_id, kind) WHERE subject_release_id IS NOT NULL;

-- ---------- outbox ----------
--
-- Transactional outbox. The application inserts an outbox row in the
-- same DB transaction as the aggregate change that produced the side
-- effect. A background worker (Module 10) polls
--   WHERE status = 'pending' AND next_attempt_at <= now()
-- ordering by next_attempt_at, and dispatches each row to Confluence /
-- webhook receivers / etc. This guarantees at-least-once delivery
-- without depending on external systems being up at write time.

CREATE TABLE outbox (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type      text        NOT NULL CHECK (length(aggregate_type) > 0),
    aggregate_id        uuid        NOT NULL,
    event_type          text        NOT NULL CHECK (length(event_type) > 0),
    payload             jsonb       NOT NULL,
    status              text        NOT NULL DEFAULT 'pending'
                                        CHECK (status IN ('pending','dispatched','failed')),
    attempts            integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error          text        NOT NULL DEFAULT '',
    next_attempt_at     timestamptz NOT NULL DEFAULT now(),
    dispatched_at       timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT outbox_dispatched_consistency CHECK (
        (status = 'dispatched' AND dispatched_at IS NOT NULL) OR
        (status <> 'dispatched' AND dispatched_at IS NULL)
    )
);

-- Worker hot-path lookup index.
CREATE INDEX outbox_pending_idx
    ON outbox (next_attempt_at)
    WHERE status = 'pending';

COMMIT;
