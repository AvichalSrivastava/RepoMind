package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	apigithub "github.com/repomind/repomind/internal/application/github"
	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/shared"
)

// PRAnalyser is the analyser-shaped interface the PR-analysis handler
// depends on. M5 (risk) and M6 (qa) both satisfy it: they take a PR
// and return an AnalysisResult ready to persist. Keeping the
// interface narrow lets us test the handler with fake analysers.
type PRAnalyser interface {
	Analyze(ctx context.Context, pr *pullrequest.PullRequest) (*analysis.AnalysisResult, error)
}

// PRAnalysisHandlerDeps wires the Phase 4 handler. All fields are
// required.
type PRAnalysisHandlerDeps struct {
	GitHub       apigithub.Client       // PR fetch
	Stores       persistence.Stores     // Repo/PR/Analysis persistence
	UoW          persistence.UnitOfWork // atomic Repo+PR upsert
	RiskAnalyser PRAnalyser             // M5
	QAAnalyser   PRAnalyser             // M6
	Logger       *slog.Logger
	Clock        shared.Clock // testable; defaults to SystemClock
}

// PRAnalysisHandler runs the per-PR risk + QA analysis pipeline.
//
// Sequence:
//  1. Decode the outbox payload to find the (repo, number) target.
//  2. Delegate to PRIngestor to ensure Repository + PullRequest rows
//     exist and reflect the current GitHub state. Atomic via UoW so
//     the analysers below always see a row.
//  3. OUTSIDE any DB tx: run the risk and QA analysers (each is an
//     LLM call that can take seconds). LLM calls do not hold an open
//     Postgres tx open -- we save each Analysis row independently.
//  4. Save each Analysis result. Failures of one analyser do NOT
//     poison the other: we collect errors and return a multi-error
//     so the dispatcher retries the whole event. The outbox dedup
//     key (head SHA) protects against duplicate work on retry.
//
// What this handler is NOT:
//   - It does not publish anywhere (Confluence is sprint-scope, not
//     PR-scope).
//   - It does not chain into further events. The Analysis rows it
//     writes are the durable artefact; release-notes composition
//     reads them later.
type PRAnalysisHandler struct {
	deps     PRAnalysisHandlerDeps
	ingestor *PRIngestor
}

// NewPRAnalysisHandler validates deps and constructs the handler.
func NewPRAnalysisHandler(deps PRAnalysisHandlerDeps) (*PRAnalysisHandler, error) {
	switch {
	case deps.GitHub == nil:
		return nil, errors.New("orchestration: PRAnalysisHandler: GitHub is required")
	case deps.Stores.Repos == nil:
		return nil, errors.New("orchestration: PRAnalysisHandler: Stores.Repos is required")
	case deps.Stores.PullRequests == nil:
		return nil, errors.New("orchestration: PRAnalysisHandler: Stores.PullRequests is required")
	case deps.Stores.Analyses == nil:
		return nil, errors.New("orchestration: PRAnalysisHandler: Stores.Analyses is required")
	case deps.UoW == nil:
		return nil, errors.New("orchestration: PRAnalysisHandler: UoW is required")
	case deps.RiskAnalyser == nil:
		return nil, errors.New("orchestration: PRAnalysisHandler: RiskAnalyser is required")
	case deps.QAAnalyser == nil:
		return nil, errors.New("orchestration: PRAnalysisHandler: QAAnalyser is required")
	case deps.Logger == nil:
		return nil, errors.New("orchestration: PRAnalysisHandler: Logger is required")
	}
	if deps.Clock == nil {
		deps.Clock = shared.SystemClock{}
	}
	ingestor, err := NewPRIngestor(PRIngestorDeps{
		GitHub: deps.GitHub,
		Stores: deps.Stores,
		UoW:    deps.UoW,
		Logger: deps.Logger,
		Clock:  deps.Clock,
	})
	if err != nil {
		return nil, err
	}
	return &PRAnalysisHandler{deps: deps, ingestor: ingestor}, nil
}

// Compile-time check.
var _ Handler = (*PRAnalysisHandler)(nil)

// Handle implements the Phase 4 pipeline.
func (h *PRAnalysisHandler) Handle(ctx context.Context, evt persistence.OutboxEvent) error {
	if evt.EventType != string(EventPRAnalysisRequested) {
		return fmt.Errorf("orchestration: PRAnalysisHandler: unexpected event_type=%q", evt.EventType)
	}

	var payload PRAnalysisRequestPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return fmt.Errorf("orchestration: PRAnalysisHandler: decode payload: %w", err)
	}
	if payload.Repo.Owner == "" || payload.Repo.Name == "" || payload.Number <= 0 || payload.HeadSHA == "" {
		// Defensive -- the enqueuer already validates these, but
		// the outbox row is durable so it's worth re-checking on
		// the consume side.
		return fmt.Errorf("orchestration: PRAnalysisHandler: incomplete payload: %+v", payload)
	}

	logger := h.deps.Logger.With(
		"event_id", evt.ID.String(),
		"repo", payload.Repo.Owner+"/"+payload.Repo.Name,
		"pr_number", payload.Number,
		"head_sha", payload.HeadSHA,
		"delivery_id", payload.DeliveryID,
	)

	// Step 1+2: ensure Repo + PR rows. Atomic via UoW.
	pr, err := h.upsertRepoAndPR(ctx, payload)
	if err != nil {
		return fmt.Errorf("orchestration: PRAnalysisHandler: upsert: %w", err)
	}
	logger.Info("PRAnalysisHandler: PR upserted", "pr_id", pr.ID().String())

	// Step 3: run the analysers (outside any tx). Each is an
	// independent LLM call; one failing shouldn't block the other.
	var multiErr []string

	if err := h.runAndSaveAnalysis(ctx, logger, "risk", h.deps.RiskAnalyser, pr); err != nil {
		multiErr = append(multiErr, "risk: "+err.Error())
	}
	if err := h.runAndSaveAnalysis(ctx, logger, "qa", h.deps.QAAnalyser, pr); err != nil {
		multiErr = append(multiErr, "qa: "+err.Error())
	}

	if len(multiErr) > 0 {
		// At least one analyser failed; surface as a retryable
		// error. The dispatcher will back off + retry; the dedup
		// key (head SHA) protects against duplicate LLM spend
		// for the same PR snapshot.
		return fmt.Errorf("orchestration: PRAnalysisHandler: analyser(s) failed: %s",
			strings.Join(multiErr, "; "))
	}

	logger.Info("PRAnalysisHandler: analysis complete")
	return nil
}

// upsertRepoAndPR delegates to the shared PRIngestor. The ingestor is
// also exposed as a stand-alone type so tooling (backfill CLI, admin
// replays) can re-hydrate PR rows without wiring the LLM analysers.
func (h *PRAnalysisHandler) upsertRepoAndPR(
	ctx context.Context,
	payload PRAnalysisRequestPayload,
) (*pullrequest.PullRequest, error) {
	return h.ingestor.UpsertPR(ctx, payload.Repo, payload.Number)
}

// runAndSaveAnalysis is the boilerplate-eliminating wrapper for the
// "run an analyser; persist its result" pair.
func (h *PRAnalysisHandler) runAndSaveAnalysis(
	ctx context.Context,
	logger *slog.Logger,
	name string,
	analyser PRAnalyser,
	pr *pullrequest.PullRequest,
) error {
	result, err := analyser.Analyze(ctx, pr)
	if err != nil {
		logger.Error("PRAnalysisHandler: analyser failed", "analyser", name, "err", err)
		return err
	}
	if err := h.deps.Stores.Analyses.Save(ctx, result); err != nil {
		logger.Error("PRAnalysisHandler: save analysis failed", "analyser", name, "err", err)
		return err
	}
	logger.Info("PRAnalysisHandler: analysis saved",
		"analyser", name,
		"analysis_id", result.ID().String(),
	)
	return nil
}

// ---------- helpers: GitHub -> domain mappers ----------

func openFromGitHub(
	repoID shared.RepositoryID,
	ghPR *apigithub.PullRequest,
	files []apigithub.File,
	commits []apigithub.Commit,
	reviews []apigithub.Review,
	clock shared.Clock,
) (*pullrequest.PullRequest, error) {
	author, err := authorFromGitHub(ghPR.Author)
	if err != nil {
		return nil, fmt.Errorf("author: %w", err)
	}
	src, err := shared.NewBranch(ghPR.HeadBranch)
	if err != nil {
		return nil, fmt.Errorf("head branch: %w", err)
	}
	dst, err := shared.NewBranch(ghPR.BaseBranch)
	if err != nil {
		return nil, fmt.Errorf("base branch: %w", err)
	}
	pr, err := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: repoID,
		Number:       pullrequest.Number(ghPR.Number),
		Title:        ghPR.Title,
		Description:  ghPR.Body,
		Author:       author,
		SourceBranch: src,
		TargetBranch: dst,
		Labels:       ghPR.Labels,
	}, clock)
	if err != nil {
		return nil, err
	}
	if err := applyGitHubUpdate(pr, ghPR, files, commits, reviews, clock); err != nil {
		return nil, err
	}
	return pr, nil
}

// applyGitHubUpdate mutates pr to reflect ghPR + ancillary lists.
// Handles the merge/close state transitions and attaches files,
// commits, reviewers.
func applyGitHubUpdate(
	pr *pullrequest.PullRequest,
	ghPR *apigithub.PullRequest,
	files []apigithub.File,
	commits []apigithub.Commit,
	reviews []apigithub.Review,
	clock shared.Clock,
) error {
	for _, f := range files {
		if err := pr.AttachFile(pullrequest.ChangedFile{
			Path:      f.Path,
			Status:    pullrequest.FileStatus(f.Status),
			Additions: f.Additions,
			Deletions: f.Deletions,
		}); err != nil {
			return fmt.Errorf("attach file %q: %w", f.Path, err)
		}
	}
	for _, c := range commits {
		sha, err := shared.NewSHA(c.SHA)
		if err != nil {
			return fmt.Errorf("commit sha %q: %w", c.SHA, err)
		}
		author, err := authorFromGitHub(c.Author)
		if err != nil {
			return fmt.Errorf("commit author: %w", err)
		}
		if err := pr.AttachCommit(pullrequest.Commit{
			SHA: sha, Message: c.Message, Author: author, Committed: c.Timestamp,
		}); err != nil {
			return fmt.Errorf("attach commit %q: %w", c.SHA, err)
		}
	}
	for _, rv := range reviews {
		author, err := authorFromGitHub(rv.Reviewer)
		if err != nil {
			return fmt.Errorf("review author: %w", err)
		}
		state := mapReviewState(rv.State)
		if state == "" {
			continue // unknown state -- skip safely
		}
		if err := pr.RecordReview(pullrequest.Reviewer{
			Author: author, State: state, At: rv.SubmittedAt,
		}); err != nil {
			return fmt.Errorf("record review: %w", err)
		}
	}
	if len(ghPR.Labels) > 0 {
		pr.ReplaceLabels(ghPR.Labels, clock.Now())
	}

	// State transitions (idempotent on the aggregate).
	switch {
	case ghPR.Merged && ghPR.MergedAt != nil && ghPR.MergeSHA != "":
		sha, err := shared.NewSHA(ghPR.MergeSHA)
		if err != nil {
			return fmt.Errorf("merge sha %q: %w", ghPR.MergeSHA, err)
		}
		if err := pr.Merge(*ghPR.MergedAt, sha); err != nil {
			// Conflict (already merged with different SHA) is a
			// data inconsistency -- log and continue (the prior
			// merge stands).
			return fmt.Errorf("merge: %w", err)
		}
	case ghPR.State == apigithub.PRStateClosed && ghPR.ClosedAt != nil && !pr.IsMerged():
		if err := pr.Close(*ghPR.ClosedAt); err != nil {
			return fmt.Errorf("close: %w", err)
		}
	}

	// Associate Jira ticket keys mined from PR metadata. Without
	// this step, release_notes_handler's PullRequests.ListByTickets
	// join would always return empty even for correctly-ingested
	// PRs. Sources scanned (in priority order for first-seen
	// dedup): title, body, head branch, commit messages.
	sources := make([]string, 0, 3+len(commits))
	sources = append(sources, ghPR.Title, ghPR.Body, ghPR.HeadBranch)
	for _, c := range commits {
		sources = append(sources, c.Message)
	}
	for _, tk := range extractTicketKeys(sources...) {
		pr.AssociateTicket(tk) // idempotent: skips duplicates
	}
	return nil
}

func authorFromGitHub(u apigithub.UserRef) (shared.Author, error) {
	login := u.Login
	if login == "" {
		login = "ghost"
	}
	name := u.Name
	if name == "" {
		name = login
	}
	// Email is not exposed in webhook payloads; use a placeholder.
	return shared.NewAuthor(login, name, login+"@users.noreply.github.com")
}

func mapReviewState(s string) pullrequest.ReviewState {
	switch strings.ToUpper(s) {
	case "APPROVED":
		return pullrequest.ReviewApproved
	case "CHANGES_REQUESTED":
		return pullrequest.ReviewChangesRequested
	case "COMMENTED":
		return pullrequest.ReviewCommented
	case "DISMISSED":
		return pullrequest.ReviewDismissed
	default:
		return ""
	}
}

// ---------- GitHub paging helpers ----------

// pageLimit is the per-call max we use for files/commits/reviews.
// GitHub's max is 100; we mirror that to keep round-trips minimal.
const pageLimit = 100

func fetchAllFiles(ctx context.Context, gh apigithub.Client, repo RepoRef, number int) ([]apigithub.File, error) {
	var out []apigithub.File
	cursor := ""
	for {
		page, err := gh.ListPullRequestFiles(ctx, apigithub.RepositoryRef{
			Owner: repo.Owner, Name: repo.Name,
		}, number, apigithub.PageOptions{Cursor: cursor, Limit: pageLimit})
		if err != nil {
			return nil, err
		}
		if page == nil {
			break
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

func fetchAllCommits(ctx context.Context, gh apigithub.Client, repo RepoRef, number int) ([]apigithub.Commit, error) {
	var out []apigithub.Commit
	cursor := ""
	for {
		page, err := gh.ListPullRequestCommits(ctx, apigithub.RepositoryRef{
			Owner: repo.Owner, Name: repo.Name,
		}, number, apigithub.PageOptions{Cursor: cursor, Limit: pageLimit})
		if err != nil {
			return nil, err
		}
		if page == nil {
			break
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

func fetchAllReviews(ctx context.Context, gh apigithub.Client, repo RepoRef, number int) ([]apigithub.Review, error) {
	var out []apigithub.Review
	cursor := ""
	for {
		page, err := gh.ListPullRequestReviews(ctx, apigithub.RepositoryRef{
			Owner: repo.Owner, Name: repo.Name,
		}, number, apigithub.PageOptions{Cursor: cursor, Limit: pageLimit})
		if err != nil {
			return nil, err
		}
		if page == nil {
			break
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}
