package orchestration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	apigithub "github.com/repomind/repomind/internal/application/github"
	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/repository"
	"github.com/repomind/repomind/internal/domain/shared"
)

// PRIngestor is the analyser-free half of the PR-analysis pipeline:
// fetch a PR from GitHub, upsert Repository + PullRequest atomically,
// and return the persisted aggregate. It is the reusable core that
// PRAnalysisHandler delegates to, and that stand-alone tools (backfill
// CLI, admin replays, migrations) call directly to re-hydrate PR rows
// without paying for LLM analysers.
//
// The ingestor deliberately does NOT know about outbox events,
// analysers, or scheduling. Its single job is "make the PullRequest
// aggregate in Postgres reflect the current GitHub state (including
// mined Jira ticket keys via applyGitHubUpdate)".
type PRIngestor struct {
	deps PRIngestorDeps
}

// PRIngestorDeps wires the ingestor. All fields are required except
// Clock, which defaults to shared.SystemClock.
type PRIngestorDeps struct {
	GitHub apigithub.Client
	Stores persistence.Stores     // Repos + PullRequests
	UoW    persistence.UnitOfWork // atomic Repo+PR upsert
	Logger *slog.Logger
	Clock  shared.Clock
}

// NewPRIngestor validates deps and constructs an ingestor.
func NewPRIngestor(deps PRIngestorDeps) (*PRIngestor, error) {
	switch {
	case deps.GitHub == nil:
		return nil, errors.New("orchestration: PRIngestor: GitHub is required")
	case deps.Stores.Repos == nil:
		return nil, errors.New("orchestration: PRIngestor: Stores.Repos is required")
	case deps.Stores.PullRequests == nil:
		return nil, errors.New("orchestration: PRIngestor: Stores.PullRequests is required")
	case deps.UoW == nil:
		return nil, errors.New("orchestration: PRIngestor: UoW is required")
	case deps.Logger == nil:
		return nil, errors.New("orchestration: PRIngestor: Logger is required")
	}
	if deps.Clock == nil {
		deps.Clock = shared.SystemClock{}
	}
	return &PRIngestor{deps: deps}, nil
}

// UpsertPR fetches (owner/name#number) from GitHub, then inside a
// single UnitOfWork ensures the Repository row exists and the
// PullRequest aggregate is created-or-updated to match. Returns the
// persisted PR aggregate.
//
// The GitHub round-trips (get PR, list files, commits, reviews) happen
// BEFORE the transaction so no Postgres connection is held open across
// the network calls.
func (i *PRIngestor) UpsertPR(ctx context.Context, ref RepoRef, number int) (*pullrequest.PullRequest, error) {
	if ref.Owner == "" || ref.Name == "" {
		return nil, fmt.Errorf("orchestration: PRIngestor: repo owner/name required, got %+v", ref)
	}
	if number <= 0 {
		return nil, fmt.Errorf("orchestration: PRIngestor: pr number must be > 0, got %d", number)
	}

	ghPR, err := i.deps.GitHub.GetPullRequest(ctx, apigithub.RepositoryRef{
		Owner: ref.Owner, Name: ref.Name,
	}, number)
	if err != nil {
		return nil, fmt.Errorf("github: get pr: %w", err)
	}
	if ghPR == nil {
		return nil, fmt.Errorf("github: get pr: nil result")
	}

	files, err := fetchAllFiles(ctx, i.deps.GitHub, ref, number)
	if err != nil {
		return nil, fmt.Errorf("github: list files: %w", err)
	}
	commits, err := fetchAllCommits(ctx, i.deps.GitHub, ref, number)
	if err != nil {
		return nil, fmt.Errorf("github: list commits: %w", err)
	}
	reviews, err := fetchAllReviews(ctx, i.deps.GitHub, ref, number)
	if err != nil {
		return nil, fmt.Errorf("github: list reviews: %w", err)
	}

	var pr *pullrequest.PullRequest
	err = i.deps.UoW.Within(ctx, func(ctx context.Context, s persistence.Stores) error {
		repo, err := i.resolveRepository(ctx, s, ref)
		if err != nil {
			return err
		}

		existing, err := s.PullRequests.FindByRepoNumber(ctx, repo.ID(), pullrequest.Number(number))
		switch {
		case err == nil:
			pr = existing
			if err := applyGitHubUpdate(pr, ghPR, files, commits, reviews, i.deps.Clock); err != nil {
				return err
			}
		case errors.Is(err, persistence.ErrNotFound):
			pr, err = openFromGitHub(repo.ID(), ghPR, files, commits, reviews, i.deps.Clock)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("find pr: %w", err)
		}

		if err := s.PullRequests.Save(ctx, pr); err != nil {
			return fmt.Errorf("save pr: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pr, nil
}

// resolveRepository returns an existing Repository row or inserts a
// new one based on owner/name. Called inside a UoW; Repos.Save handles
// idempotency at the adapter level.
func (i *PRIngestor) resolveRepository(
	ctx context.Context,
	s persistence.Stores,
	ref RepoRef,
) (*repository.Repository, error) {
	existing, err := s.Repos.FindByFullName(ctx, ref.Owner, ref.Name)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, persistence.ErrNotFound) {
		return nil, fmt.Errorf("find repo: %w", err)
	}
	fresh, err := repository.Register(repository.RegistrationInput{
		Owner: ref.Owner,
		Name:  ref.Name,
	}, i.deps.Clock)
	if err != nil {
		return nil, fmt.Errorf("register repo: %w", err)
	}
	if err := s.Repos.Save(ctx, fresh); err != nil {
		return nil, fmt.Errorf("save repo: %w", err)
	}
	return fresh, nil
}
