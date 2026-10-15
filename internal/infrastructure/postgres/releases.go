package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/domain/release"
	"github.com/repomind/repomind/internal/domain/shared"
)

type releasesRepo struct{ q dbtx }

var _ persistence.Releases = (*releasesRepo)(nil)

const releaseSelectCols = `
	id, sprint_id, version, status, notes, pull_request_ids, created_at, published_at, content_hash
`

func (r *releasesRepo) Save(ctx context.Context, rel *release.Release) error {
	prIDs := rel.PullRequests()
	prStrings := make([]string, 0, len(prIDs))
	for _, id := range prIDs {
		prStrings = append(prStrings, id.String())
	}
	prsB, err := json.Marshal(prStrings)
	if err != nil {
		return fmt.Errorf("postgres save release: marshal pr ids: %w", err)
	}

	const q = `
INSERT INTO release (
	id, sprint_id, version, status, notes, pull_request_ids, created_at, published_at, content_hash
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''))
ON CONFLICT (sprint_id, version) DO UPDATE SET
	status           = EXCLUDED.status,
	notes            = EXCLUDED.notes,
	pull_request_ids = EXCLUDED.pull_request_ids,
	published_at     = EXCLUDED.published_at,
	content_hash     = COALESCE(EXCLUDED.content_hash, release.content_hash)`
	_, err = r.q.Exec(ctx, q,
		rel.ID().UUID(),
		rel.SprintID().UUID(),
		rel.Version().String(),
		string(rel.Status()),
		rel.Notes(),
		prsB,
		rel.CreatedAt(),
		rel.PublishedAt(),
		rel.ContentHash(),
	)
	return classify("save release", err)
}

func (r *releasesRepo) FindByID(ctx context.Context, id shared.ReleaseID) (*release.Release, error) {
	return r.findOne(ctx, "WHERE id = $1", id.UUID())
}

func (r *releasesRepo) FindBySprintVersion(
	ctx context.Context,
	sprintID shared.SprintID,
	version release.Version,
) (*release.Release, error) {
	return r.findOne(ctx, "WHERE sprint_id = $1 AND version = $2", sprintID.UUID(), version.String())
}

func (r *releasesRepo) ListBySprint(
	ctx context.Context,
	sprintID shared.SprintID,
) ([]*release.Release, error) {
	q := "SELECT " + releaseSelectCols + " FROM release WHERE sprint_id = $1 ORDER BY created_at DESC"
	rows, err := r.q.Query(ctx, q, sprintID.UUID())
	if err != nil {
		return nil, classify("list releases by sprint", err)
	}
	defer rows.Close()

	var out []*release.Release
	for rows.Next() {
		rel, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	if err := rows.Err(); err != nil {
		return nil, classify("list releases: rows", err)
	}
	return out, nil
}

// FindLatestBySprint returns the most-recently-created release for
// a sprint. Returns ErrNotFound when no release exists.
func (r *releasesRepo) FindLatestBySprint(
	ctx context.Context,
	sprintID shared.SprintID,
) (*release.Release, error) {
	return r.findOne(ctx,
		"WHERE sprint_id = $1 ORDER BY created_at DESC",
		sprintID.UUID(),
	)
}

func (r *releasesRepo) findOne(ctx context.Context, where string, args ...any) (*release.Release, error) {
	rows, err := r.q.Query(ctx, "SELECT "+releaseSelectCols+" FROM release "+where+" LIMIT 1", args...)
	if err != nil {
		return nil, classify("find release", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, classify("find release: rows", err)
		}
		// Must pass pgx.ErrNoRows so classify maps to
		// persistence.ErrNotFound (see pullrequests.go comment).
		return nil, classify("find release", pgx.ErrNoRows)
	}
	return scanRelease(rows)
}

func scanRelease(rs rowScanner) (*release.Release, error) {
	var (
		rawID       uuid.UUID
		rawSprintID uuid.UUID
		versionStr  string
		statusStr   string
		notes       string
		prsB        []byte
		createdAt   time.Time
		publishedAt *time.Time
		contentHash *string
	)
	if err := rs.Scan(&rawID, &rawSprintID, &versionStr, &statusStr, &notes,
		&prsB, &createdAt, &publishedAt, &contentHash); err != nil {
		return nil, classify("scan release", err)
	}

	id, err := shared.ParseReleaseID(rawID.String())
	if err != nil {
		return nil, classify("scan release: corrupt id", err)
	}
	sprintID, err := shared.ParseSprintID(rawSprintID.String())
	if err != nil {
		return nil, classify("scan release: corrupt sprint id", err)
	}
	version, err := release.NewVersion(versionStr)
	if err != nil {
		return nil, classify("scan release: corrupt version", err)
	}
	status := release.Status(statusStr)
	if status != release.StatusDraft && status != release.StatusPublished {
		return nil, classify("scan release: corrupt status",
			errors.New("invalid status "+statusStr))
	}

	var prStrings []string
	if err := unmarshalNonNull(prsB, &prStrings); err != nil {
		return nil, classify("scan release: pr ids", err)
	}
	prIDs := make([]shared.PullRequestID, 0, len(prStrings))
	for _, s := range prStrings {
		pid, err := shared.ParsePullRequestID(s)
		if err != nil {
			return nil, classify("scan release: pr id "+s, err)
		}
		prIDs = append(prIDs, pid)
	}

	hash := ""
	if contentHash != nil {
		hash = *contentHash
	}
	return release.Rehydrate(id, sprintID, version, status, prIDs, notes,
		hash, createdAt.UTC(), ptrUTC(publishedAt)), nil
}
