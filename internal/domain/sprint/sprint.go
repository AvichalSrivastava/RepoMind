// Package sprint contains the Sprint aggregate — a Jira-mirrored,
// time-boxed iteration that scopes work to a team and a set of pull
// requests. In V1, Jira is the authoritative source of truth: every
// Sprint in RepoMind is a mirror of a Jira sprint. RepoMind never
// plans sprints internally.
package sprint

import (
	"fmt"
	"strings"
	"time"

	"github.com/repomind/repomind/internal/domain/shared"
)

// State mirrors Jira's sprint state vocabulary.
type State string

const (
	StateFuture State = "future"
	StateActive State = "active"
	StateClosed State = "closed"
)

// Valid reports whether s is one of the known sprint states.
func (s State) Valid() bool {
	switch s {
	case StateFuture, StateActive, StateClosed:
		return true
	}
	return false
}

// Sprint is the aggregate root.
//
// Invariants:
//   - JiraSprintID > 0 (and unique across the system in persistence).
//   - BoardID > 0.
//   - Team is non-empty.
//   - Name is non-empty.
//   - State ∈ {future, active, closed}.
//   - If both StartsAt and EndsAt are set, StartsAt < EndsAt.
//   - CompletedAt is set iff State == Closed.
type Sprint struct {
	id           shared.SprintID
	jiraSprintID int64
	boardID      int64
	team         string
	name         string
	state        State
	goal         string
	startsAt     *time.Time
	endsAt       *time.Time
	completedAt  *time.Time
	mirroredAt   time.Time

	emitter shared.EventEmitter
}

// MirrorInput captures the Jira-side state needed to mirror a sprint.
type MirrorInput struct {
	JiraSprintID int64
	BoardID      int64
	Team         string
	Name         string
	State        State
	Goal         string
	StartsAt     *time.Time
	EndsAt       *time.Time
	CompletedAt  *time.Time
}

// MirrorFromJira creates a new Sprint mirrored from Jira.
func MirrorFromJira(in MirrorInput, clock shared.Clock) (*Sprint, error) {
	if err := validateMirrorInput(in); err != nil {
		return nil, err
	}
	now := clock.Now()
	s := &Sprint{
		id:           shared.NewSprintID(),
		jiraSprintID: in.JiraSprintID,
		boardID:      in.BoardID,
		team:         strings.TrimSpace(in.Team),
		name:         strings.TrimSpace(in.Name),
		state:        in.State,
		goal:         strings.TrimSpace(in.Goal),
		startsAt:     copyTime(in.StartsAt),
		endsAt:       copyTime(in.EndsAt),
		completedAt:  copyTime(in.CompletedAt),
		mirroredAt:   now,
	}
	s.emitter.Record(MirroredEvent{
		BaseEvent:    shared.NewBaseEvent(s.id.String(), now),
		JiraSprintID: in.JiraSprintID,
		State:        string(in.State),
	})
	return s, nil
}

// Rehydrate reconstructs a Sprint from persisted state.
func Rehydrate(
	id shared.SprintID,
	jiraSprintID, boardID int64,
	team, name, goal string,
	state State,
	startsAt, endsAt, completedAt *time.Time,
	mirroredAt time.Time,
) *Sprint {
	return &Sprint{
		id: id, jiraSprintID: jiraSprintID, boardID: boardID,
		team: team, name: name, goal: goal, state: state,
		startsAt: startsAt, endsAt: endsAt, completedAt: completedAt,
		mirroredAt: mirroredAt,
	}
}

// ---------- Accessors ----------

func (s *Sprint) ID() shared.SprintID              { return s.id }
func (s *Sprint) JiraSprintID() int64              { return s.jiraSprintID }
func (s *Sprint) BoardID() int64                   { return s.boardID }
func (s *Sprint) Team() string                     { return s.team }
func (s *Sprint) Name() string                     { return s.name }
func (s *Sprint) Goal() string                     { return s.goal }
func (s *Sprint) State() State                     { return s.state }
func (s *Sprint) StartsAt() *time.Time             { return copyTime(s.startsAt) }
func (s *Sprint) EndsAt() *time.Time               { return copyTime(s.endsAt) }
func (s *Sprint) CompletedAt() *time.Time          { return copyTime(s.completedAt) }
func (s *Sprint) MirroredAt() time.Time            { return s.mirroredAt }
func (s *Sprint) PullEvents() []shared.DomainEvent { return s.emitter.PullEvents() }

// ---------- Behavior ----------

// SyncFromJira refreshes mutable fields from a fresh Jira observation
// of the same sprint. Emits StateChangedEvent only when state actually
// transitions.
func (s *Sprint) SyncFromJira(in MirrorInput, at time.Time) error {
	if err := validateMirrorInput(in); err != nil {
		return err
	}
	if in.JiraSprintID != s.jiraSprintID {
		return fmt.Errorf("%w: cannot sync sprint with different Jira id", shared.ErrInvariant)
	}
	prev := s.state

	s.boardID = in.BoardID
	s.team = strings.TrimSpace(in.Team)
	s.name = strings.TrimSpace(in.Name)
	s.goal = strings.TrimSpace(in.Goal)
	s.state = in.State
	s.startsAt = copyTime(in.StartsAt)
	s.endsAt = copyTime(in.EndsAt)
	s.completedAt = copyTime(in.CompletedAt)
	s.mirroredAt = at

	if prev != in.State {
		s.emitter.Record(StateChangedEvent{
			BaseEvent: shared.NewBaseEvent(s.id.String(), at),
			From:      string(prev),
			To:        string(in.State),
		})
	}
	return nil
}

func validateMirrorInput(in MirrorInput) error {
	if in.JiraSprintID <= 0 {
		return shared.NewValidationError("jira_sprint_id", "must be positive")
	}
	if in.BoardID <= 0 {
		return shared.NewValidationError("board_id", "must be positive")
	}
	if strings.TrimSpace(in.Team) == "" {
		return shared.NewValidationError("team", "must not be empty")
	}
	if strings.TrimSpace(in.Name) == "" {
		return shared.NewValidationError("name", "must not be empty")
	}
	if !in.State.Valid() {
		return shared.NewValidationError("state", fmt.Sprintf("invalid sprint state %q", in.State))
	}
	if in.StartsAt != nil && in.EndsAt != nil && !in.StartsAt.Before(*in.EndsAt) {
		return fmt.Errorf("%w: starts_at must precede ends_at", shared.ErrInvariant)
	}
	if in.State == StateClosed && in.CompletedAt == nil {
		return shared.NewValidationError("completed_at", "must be set when state=closed")
	}
	if in.State != StateClosed && in.CompletedAt != nil {
		return shared.NewValidationError("completed_at", "must be nil when state != closed")
	}
	return nil
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

// ---------- Events ----------

// MirroredEvent fires when a Jira sprint is first mirrored into RepoMind.
type MirroredEvent struct {
	shared.BaseEvent
	JiraSprintID int64
	State        string
}

func (MirroredEvent) EventName() string { return "sprint.mirrored" }

// StateChangedEvent fires when a subsequent SyncFromJira observes a
// state transition (e.g. active → closed).
type StateChangedEvent struct {
	shared.BaseEvent
	From string
	To   string
}

func (StateChangedEvent) EventName() string { return "sprint.state_changed" }
