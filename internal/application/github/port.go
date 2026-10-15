// Package github defines the application-layer port for GitHub. It is
// the single boundary between RepoMind use-cases and any GitHub REST /
// GraphQL implementation. The concrete adapter lives in
// internal/infrastructure/github.
//
// The port deliberately exposes only what release-note generation needs
// today (V1): listing and reading merged pull requests, their files,
// commits, and reviews. Anything else (issues, branches, statuses,
// commits API, search) is intentionally out of scope.
package github

import (
	"context"
	"time"
)

// Client is the read-only GitHub API the application depends on.
//
// All methods are safe for concurrent use. Implementations MUST honour
// ctx cancellation and return wrapped sentinel errors from
// infrastructure/github (e.g. errors.Is(err, ErrNotFound)).
type Client interface {
	// ListPullRequests returns merged + open pull requests for a repo,
	// most-recently-updated first. Use ListPROptions.State to filter.
	ListPullRequests(ctx context.Context, repo RepositoryRef, opts ListPROptions) (*PullRequestPage, error)

	// GetPullRequest fetches a single PR by number with full details.
	GetPullRequest(ctx context.Context, repo RepositoryRef, number int) (*PullRequest, error)

	// ListPullRequestFiles returns the files changed by a PR (paginated).
	ListPullRequestFiles(ctx context.Context, repo RepositoryRef, number int, page PageOptions) (*FilePage, error)

	// ListPullRequestCommits returns the commits attached to a PR.
	ListPullRequestCommits(ctx context.Context, repo RepositoryRef, number int, page PageOptions) (*CommitPage, error)

	// ListPullRequestReviews returns submitted reviews for a PR.
	ListPullRequestReviews(ctx context.Context, repo RepositoryRef, number int, page PageOptions) (*ReviewPage, error)
}

// WebhookVerifier validates the X-Hub-Signature-256 header against the
// raw body of an incoming GitHub webhook.
//
// Implementations MUST use constant-time comparison to avoid leaking
// information through timing.
type WebhookVerifier interface {
	Verify(signatureHeader string, body []byte) error
}

// BranchPolicy filters pull requests by their target branch. The
// concrete implementation lives in internal/infrastructure/github
// (RepoPolicy backed by a YAML file); this port keeps the webhook
// + dispatcher layers free of infrastructure imports.
//
// A nil receiver MUST be safe to invoke and MUST return true (no
// filtering configured = accept everything). Implementations should
// be safe for concurrent use.
type BranchPolicy interface {
	// Accept reports whether a PR in `owner/name` targeting
	// `branch` is in scope. Comparison is case-insensitive.
	Accept(owner, name, branch string) bool
}

// ---------- Identifiers / paging ----------

// RepositoryRef identifies a GitHub repository by its `owner/name` pair.
type RepositoryRef struct {
	Owner string
	Name  string
}

// PageOptions is an opaque cursor + per-page limit. Callers MUST NOT
// inspect or fabricate Cursor values; they are produced by the
// adapter's Next* fields.
type PageOptions struct {
	Cursor string
	Limit  int
}

// ListPROptions narrows ListPullRequests results.
type ListPROptions struct {
	Page PageOptions
	// State filters by lifecycle. Empty defaults to "all".
	State PRState
	// Base filters by target branch (e.g. "main"). Empty = any branch.
	Base string
	// SortDirection: "asc" or "desc" (default desc).
	SortDirection string
}

// PRState enumerates the PR lifecycle states the port understands.
type PRState string

const (
	PRStateAll    PRState = "all"
	PRStateOpen   PRState = "open"
	PRStateClosed PRState = "closed"
)

// ---------- DTOs ----------

// PullRequest is the minimum projection RepoMind needs for analysis.
type PullRequest struct {
	Number    int
	Title     string
	Body      string
	State     PRState
	Merged    bool
	MergedAt  *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
	ClosedAt  *time.Time

	BaseBranch string
	HeadBranch string
	MergeSHA   string // empty if not merged

	Author       UserRef
	Labels       []string
	Additions    int
	Deletions    int
	ChangedFiles int

	URL string // html_url for linking from Confluence
}

// UserRef is the minimal user reference. We do not store emails.
type UserRef struct {
	Login string
	Name  string // may be empty for bot/orgs
}

// File is a file changed by a pull request.
type File struct {
	Path         string
	Status       string // added | modified | removed | renamed
	Additions    int
	Deletions    int
	Changes      int
	PreviousPath string // populated when Status == "renamed"
}

// Commit is a commit attached to a pull request.
type Commit struct {
	SHA       string
	Message   string
	Author    UserRef
	Committer UserRef
	Timestamp time.Time
}

// Review is a submitted review on a pull request.
type Review struct {
	ID          int64
	Reviewer    UserRef
	State       string // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED
	Body        string
	SubmittedAt time.Time
}

// ---------- Page envelopes ----------

// PullRequestPage is one page of pull request results.
type PullRequestPage struct {
	Items      []PullRequest
	NextCursor string
}

// FilePage is one page of file results.
type FilePage struct {
	Items      []File
	NextCursor string
}

// CommitPage is one page of commit results.
type CommitPage struct {
	Items      []Commit
	NextCursor string
}

// ReviewPage is one page of review results.
type ReviewPage struct {
	Items      []Review
	NextCursor string
}
