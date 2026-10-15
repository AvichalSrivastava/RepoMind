package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/repomind/repomind/internal/domain/sprint"
)

// sprintsRepo is the postgres implementation of persistence.Sprints.
type sprintsRepo struct{ q dbtx }

// Compile-time assertion.
var _ persistence.Sprints = (*sprintsRepo)(nil)

const sprintSelectCols = `
	id, jira_sprint_id, board_id, team, name, state, goal,
	starts_at, ends_at, completed_at, mirrored_at
`

func (r *sprintsRepo) Save(ctx context.Context, s *sprint.Sprint) error {
	const q = `
INSERT INTO sprint (
	id, jira_sprint_id, board_id, team, name, state, goal,
	starts_at, ends_at, completed_at, mirrored_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
ON CONFLICT (jira_sprint_id) DO UPDATE SET
	board_id     = EXCLUDED.board_id,
	team         = EXCLUDED.team,
	name         = EXCLUDED.name,
	state        = EXCLUDED.state,
	goal         = EXCLUDED.goal,
	starts_at    = EXCLUDED.starts_at,
	ends_at      = EXCLUDED.ends_at,
	completed_at = EXCLUDED.completed_at,
	mirrored_at  = EXCLUDED.mirrored_at`
	_, err := r.q.Exec(ctx, q,
		s.ID().UUID(),
		s.JiraSprintID(),
		s.BoardID(),
		s.Team(),
		s.Name(),
		string(s.State()),
		s.Goal(),
		s.StartsAt(),
		s.EndsAt(),
		s.CompletedAt(),
		s.MirroredAt(),
	)
	return classify("save sprint", err)
}

func (r *sprintsRepo) FindByID(ctx context.Context, id shared.SprintID) (*sprint.Sprint, error) {
	return r.findOne(ctx, "WHERE id = $1", id.UUID())
}

func (r *sprintsRepo) FindByJiraID(ctx context.Context, jiraID int64) (*sprint.Sprint, error) {
	return r.findOne(ctx, "WHERE jira_sprint_id = $1", jiraID)
}

func (r *sprintsRepo) FindActiveByTeam(ctx context.Context, team string) (*sprint.Sprint, error) {
	return r.findOne(ctx, `WHERE team = $1 AND state = 'active'
		ORDER BY mirrored_at DESC LIMIT 1`, team)
}

func (r *sprintsRepo) findOne(ctx context.Context, where string, args ...any) (*sprint.Sprint, error) {
	row := r.q.QueryRow(ctx, "SELECT "+sprintSelectCols+" FROM sprint "+where, args...)

	var (
		rawID        uuid.UUID
		jiraSprintID int64
		boardID      int64
		team         string
		name         string
		state        string
		goal         string
		startsAt     *time.Time
		endsAt       *time.Time
		completedAt  *time.Time
		mirroredAt   time.Time
	)
	if err := row.Scan(
		&rawID, &jiraSprintID, &boardID, &team, &name, &state, &goal,
		&startsAt, &endsAt, &completedAt, &mirroredAt,
	); err != nil {
		return nil, classify("find sprint", err)
	}
	id, err := shared.ParseSprintID(rawID.String())
	if err != nil {
		return nil, classify("find sprint: corrupt id", err)
	}
	st := sprint.State(state)
	if !st.Valid() {
		return nil, classify("find sprint: corrupt state",
			errors.New("invalid state "+state))
	}
	return sprint.Rehydrate(id, jiraSprintID, boardID, team, name, goal, st,
		ptrUTC(startsAt), ptrUTC(endsAt), ptrUTC(completedAt), mirroredAt.UTC()), nil
}

func ptrUTC(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}
