package persistence

import (
	"context"
	"time"

	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/release"
	"github.com/repomind/repomind/internal/domain/repository"
	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/repomind/repomind/internal/domain/sprint"
)

// Sprints is the persistence port for the Sprint aggregate.
type Sprints interface {
	// Save inserts a new Sprint or updates an existing one. Identity
	// is the aggregate ID. The adapter uses INSERT … ON CONFLICT
	// (jira_sprint_id) DO UPDATE to keep the Jira mirror consistent
	// across concurrent mirror operations.
	Save(ctx context.Context, s *sprint.Sprint) error

	// FindByID returns the sprint with the given aggregate ID.
	// Returns ErrNotFound when no row matches.
	FindByID(ctx context.Context, id shared.SprintID) (*sprint.Sprint, error)

	// FindByJiraID returns the sprint whose jira_sprint_id matches.
	// This is the canonical lookup when mirroring from Jira.
	FindByJiraID(ctx context.Context, jiraID int64) (*sprint.Sprint, error)

	// FindActiveByTeam returns the single active sprint for a team.
	// Returns ErrNotFound when no active sprint exists. If somehow
	// more than one is active (data error), the most recently
	// mirrored one is returned.
	FindActiveByTeam(ctx context.Context, team string) (*sprint.Sprint, error)
}

// Repos is the persistence port for the Repository aggregate. (Named
// `Repos` rather than `Repositories` to avoid the "RepositoryRepository"
// awkwardness common in Java-style DDD.)
type Repos interface {
	Save(ctx context.Context, r *repository.Repository) error
	FindByID(ctx context.Context, id shared.RepositoryID) (*repository.Repository, error)
	FindByFullName(ctx context.Context, owner, name string) (*repository.Repository, error)
}

// PullRequests is the persistence port for the PullRequest aggregate.
type PullRequests interface {
	Save(ctx context.Context, pr *pullrequest.PullRequest) error
	FindByID(ctx context.Context, id shared.PullRequestID) (*pullrequest.PullRequest, error)
	FindByRepoNumber(
		ctx context.Context,
		repoID shared.RepositoryID,
		number pullrequest.Number,
	) (*pullrequest.PullRequest, error)

	// ListByTickets returns PRs whose tickets[] jsonb array overlaps
	// the given set of TicketKeys. Used by the sprint worker to find
	// every PR referencing any ticket in the active sprint.
	// Empty input returns an empty slice without hitting the DB.
	ListByTickets(ctx context.Context, tickets []shared.TicketKey) ([]*pullrequest.PullRequest, error)

	// ListMergedWithoutTicketsInWindow returns every merged PR whose
	// tickets[] jsonb array is empty AND merged_at falls in
	// [start, end]. Used by the release-notes handler to surface
	// PRs that landed during a sprint but couldn't be attributed to
	// any Jira ticket (title, body, branch, and commit messages
	// all had no ticket key). Result is ordered by merged_at DESC.
	ListMergedWithoutTicketsInWindow(
		ctx context.Context,
		start, end time.Time,
	) ([]*pullrequest.PullRequest, error)
}

// Releases is the persistence port for the Release aggregate.
type Releases interface {
	Save(ctx context.Context, r *release.Release) error
	FindByID(ctx context.Context, id shared.ReleaseID) (*release.Release, error)
	FindBySprintVersion(
		ctx context.Context,
		sprintID shared.SprintID,
		version release.Version,
	) (*release.Release, error)
	ListBySprint(ctx context.Context, sprintID shared.SprintID) ([]*release.Release, error)

	// FindLatestBySprint returns the most-recently-created release
	// for a sprint. Used by the M10 release-notes handler for the L3
	// content-hash short-circuit (skip republish when the inputs
	// haven't changed since the last run). Returns ErrNotFound when
	// no release exists for the sprint.
	FindLatestBySprint(ctx context.Context, sprintID shared.SprintID) (*release.Release, error)
}

// Analyses is the persistence port for AnalysisResult.
//
// Note: domain-side AnalysisResult.Payload is a polymorphic interface
// (RiskReport | ReleaseNotes | QARecommendations | …). The adapter
// stores it as raw jsonb and rehydrates by dispatching on the stored
// `kind` column — see analysis.UnmarshalPayload. Adding a new Kind
// requires extending that switch in the domain package; no adapter
// changes are needed.
type Analyses interface {
	Save(ctx context.Context, a *analysis.AnalysisResult) error
	FindByID(ctx context.Context, id shared.AnalysisID) (*analysis.AnalysisResult, error)
	ListByPullRequest(
		ctx context.Context,
		prID shared.PullRequestID,
		kind analysis.Kind, // empty = all kinds
	) ([]*analysis.AnalysisResult, error)
	ListByRelease(
		ctx context.Context,
		releaseID shared.ReleaseID,
		kind analysis.Kind,
	) ([]*analysis.AnalysisResult, error)
}

// SprintPageMapping records the Confluence page that hosts a sprint's
// release notes. Persisted separately from the sprint aggregate so
// re-mirroring / flushing sprints does not clobber the page linkage
// (see migration 000007).
type SprintPageMapping struct {
	SprintID  shared.SprintID
	PageID    string
	PageTitle string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SprintPages is the persistence port for the sprint -> Confluence
// page mapping. Used by the release-notes handler in "per-sprint
// page" mode to look up (and create-if-missing) the target page.
type SprintPages interface {
	// FindBySprintID returns the mapping for the given sprint.
	// Returns ErrNotFound when the sprint has never been published
	// to a per-sprint page.
	FindBySprintID(ctx context.Context, id shared.SprintID) (*SprintPageMapping, error)

	// Save upserts a mapping. Used on first publish (fresh row) and
	// on republishes (touches updated_at, no-op otherwise).
	Save(ctx context.Context, m *SprintPageMapping) error
}

// Stores bundles every persistence port. The composition root assembles
// one for read-only / single-aggregate use, and UnitOfWork hands one to
// the caller of Within bound to the active transaction.
type Stores struct {
	Sprints      Sprints
	Repos        Repos
	PullRequests PullRequests
	Releases     Releases
	Analyses     Analyses
	Outbox       Outbox
	WorkerState  WorkerState
	SprintPages  SprintPages
}

// UnitOfWork groups multiple repository operations into a single DB
// transaction.
//
// Usage:
//
//	err := uow.Within(ctx, func(ctx context.Context, s persistence.Stores) error {
//	    if err := s.PullRequests.Save(ctx, pr); err != nil { return err }
//	    if err := s.Outbox.Enqueue(ctx, evt);    err != nil { return err }
//	    return nil
//	})
//
// The function returns nil → commit; any non-nil error → rollback.
// Adapter implementations MUST propagate context cancellation (rollback
// on ctx.Err()) and translate driver errors via the package sentinels.
type UnitOfWork interface {
	Within(ctx context.Context, fn func(ctx context.Context, s Stores) error) error
}
