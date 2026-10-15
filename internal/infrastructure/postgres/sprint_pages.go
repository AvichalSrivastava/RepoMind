package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/domain/shared"
)

// sprintPagesRepo backs persistence.SprintPages against the
// sprint_confluence_page table (migration 000007). It is a pure
// projection store — no domain aggregate, no events, just a
// (sprint_id -> page_id) mapping used by the release-notes handler
// in per-sprint page mode.
type sprintPagesRepo struct{ q dbtx }

var _ persistence.SprintPages = (*sprintPagesRepo)(nil)

func (r *sprintPagesRepo) FindBySprintID(ctx context.Context, id shared.SprintID) (*persistence.SprintPageMapping, error) {
	const q = `
SELECT sprint_id, page_id, page_title, created_at, updated_at
FROM sprint_confluence_page
WHERE sprint_id = $1`
	var (
		rawID     uuid.UUID
		pageID    string
		pageTitle string
		createdAt time.Time
		updatedAt time.Time
	)
	if err := r.q.QueryRow(ctx, q, id.UUID()).Scan(&rawID, &pageID, &pageTitle, &createdAt, &updatedAt); err != nil {
		return nil, classify("sprint_confluence_page get", err)
	}
	parsedID, err := shared.ParseSprintID(rawID.String())
	if err != nil {
		return nil, err
	}
	return &persistence.SprintPageMapping{
		SprintID:  parsedID,
		PageID:    pageID,
		PageTitle: pageTitle,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}, nil
}

func (r *sprintPagesRepo) Save(ctx context.Context, m *persistence.SprintPageMapping) error {
	if m == nil {
		return errors.New("postgres: sprint_confluence_page: nil mapping")
	}
	if strings.TrimSpace(m.PageID) == "" {
		return errors.New("postgres: sprint_confluence_page: page_id is required")
	}
	if strings.TrimSpace(m.PageTitle) == "" {
		return errors.New("postgres: sprint_confluence_page: page_title is required")
	}
	const q = `
INSERT INTO sprint_confluence_page (sprint_id, page_id, page_title, created_at, updated_at)
VALUES ($1, $2, $3, now(), now())
ON CONFLICT (sprint_id) DO UPDATE
SET page_id    = EXCLUDED.page_id,
    page_title = EXCLUDED.page_title,
    updated_at = now()`
	if _, err := r.q.Exec(ctx, q, m.SprintID.UUID(), m.PageID, m.PageTitle); err != nil {
		return classify("sprint_confluence_page save", err)
	}
	return nil
}
