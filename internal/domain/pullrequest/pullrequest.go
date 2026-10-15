// Package pullrequest contains the PullRequest aggregate — the central
// unit of analysis for RepoMind. A PullRequest owns its changed files,
// commits, reviewers, and ticket references; nothing outside this
// package may mutate those collections directly.
package pullrequest

import (
	"fmt"
	"strings"
	"time"

	"github.com/repomind/repomind/internal/domain/shared"
)

// State models the PR lifecycle. Once Merged, a PR is terminal; once
// Closed (without merge), the PR is terminal.
type State string

const (
	StateOpen   State = "open"
	StateMerged State = "merged"
	StateClosed State = "closed"
)

func (s State) Valid() bool {
	switch s {
	case StateOpen, StateMerged, StateClosed:
		return true
	}
	return false
}

// Number is the GitHub-assigned PR number within a repository
// (#1, #2, …). It is unique per-repo, not globally.
type Number int

func (n Number) Valid() bool { return n > 0 }

// ChangedFile is a value object describing one file modified by the PR.
type ChangedFile struct {
	Path      string
	Status    FileStatus
	Additions int
	Deletions int
}

// FileStatus mirrors GitHub's file status enum.
type FileStatus string

const (
	FileAdded    FileStatus = "added"
	FileModified FileStatus = "modified"
	FileRemoved  FileStatus = "removed"
	FileRenamed  FileStatus = "renamed"
	FileCopied   FileStatus = "copied"
)

// Commit is a value object describing a commit attached to the PR.
type Commit struct {
	SHA       shared.SHA
	Message   string
	Author    shared.Author
	Committed time.Time
}

// Reviewer captures who reviewed (or was requested to review) the PR.
type Reviewer struct {
	Author shared.Author
	State  ReviewState
	At     time.Time
}

// ReviewState mirrors GitHub review states we care about.
type ReviewState string

const (
	ReviewRequested        ReviewState = "requested"
	ReviewApproved         ReviewState = "approved"
	ReviewChangesRequested ReviewState = "changes_requested"
	ReviewCommented        ReviewState = "commented"
	ReviewDismissed        ReviewState = "dismissed"
)

// PullRequest is the aggregate root.
//
// Invariants:
//   - Number is positive and unique within RepositoryID.
//   - State transitions: Open → Merged | Closed (no resurrection).
//   - MergedAt and MergeSHA are set iff State == Merged.
//   - TargetBranch is non-zero.
type PullRequest struct {
	id           shared.PullRequestID
	repositoryID shared.RepositoryID
	number       Number
	title        string
	description  string
	author       shared.Author
	sourceBranch shared.Branch
	targetBranch shared.Branch
	state        State
	labels       []string
	createdAt    time.Time
	updatedAt    time.Time
	mergedAt     *time.Time
	mergeSHA     *shared.SHA
	closedAt     *time.Time

	files     []ChangedFile
	commits   []Commit
	reviewers []Reviewer
	tickets   []shared.TicketKey

	emitter shared.EventEmitter
}

// OpenInput collects the parameters required to open a PR aggregate.
type OpenInput struct {
	RepositoryID shared.RepositoryID
	Number       Number
	Title        string
	Description  string
	Author       shared.Author
	SourceBranch shared.Branch
	TargetBranch shared.Branch
	Labels       []string
}

// Open constructs a new PullRequest in the Open state.
func Open(in OpenInput, clock shared.Clock) (*PullRequest, error) {
	if in.RepositoryID.IsZero() {
		return nil, shared.NewValidationError("repository_id", "must not be zero")
	}
	if !in.Number.Valid() {
		return nil, shared.NewValidationError("number", "must be positive")
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, shared.NewValidationError("title", "must not be empty")
	}
	if in.Author.IsZero() {
		return nil, shared.NewValidationError("author", "must be provided")
	}
	if in.SourceBranch.IsZero() {
		return nil, shared.NewValidationError("source_branch", "must be provided")
	}
	if in.TargetBranch.IsZero() {
		return nil, shared.NewValidationError("target_branch", "must be provided")
	}
	if in.SourceBranch.Equal(in.TargetBranch) {
		return nil, fmt.Errorf("%w: source and target branches must differ", shared.ErrInvariant)
	}

	now := clock.Now()
	pr := &PullRequest{
		id:           shared.NewPullRequestID(),
		repositoryID: in.RepositoryID,
		number:       in.Number,
		title:        strings.TrimSpace(in.Title),
		description:  in.Description,
		author:       in.Author,
		sourceBranch: in.SourceBranch,
		targetBranch: in.TargetBranch,
		state:        StateOpen,
		labels:       dedupeLabels(in.Labels),
		createdAt:    now,
		updatedAt:    now,
	}
	pr.emitter.Record(OpenedEvent{
		BaseEvent:    shared.NewBaseEvent(pr.id.String(), now),
		Number:       int(pr.number),
		TargetBranch: pr.targetBranch.String(),
	})
	return pr, nil
}

// Rehydrate reconstructs a PullRequest from persisted state. Only the
// persistence layer should call this.
func Rehydrate(
	id shared.PullRequestID,
	repoID shared.RepositoryID,
	number Number,
	title, description string,
	author shared.Author,
	source, target shared.Branch,
	state State,
	labels []string,
	createdAt, updatedAt time.Time,
	mergedAt, closedAt *time.Time,
	mergeSHA *shared.SHA,
	files []ChangedFile,
	commits []Commit,
	reviewers []Reviewer,
	tickets []shared.TicketKey,
) *PullRequest {
	return &PullRequest{
		id: id, repositoryID: repoID, number: number,
		title: title, description: description, author: author,
		sourceBranch: source, targetBranch: target,
		state: state, labels: labels,
		createdAt: createdAt, updatedAt: updatedAt,
		mergedAt: mergedAt, closedAt: closedAt, mergeSHA: mergeSHA,
		files: files, commits: commits, reviewers: reviewers, tickets: tickets,
	}
}

// ---------- Accessors (read-only views) ----------

func (p *PullRequest) ID() shared.PullRequestID          { return p.id }
func (p *PullRequest) RepositoryID() shared.RepositoryID { return p.repositoryID }
func (p *PullRequest) Number() Number                    { return p.number }
func (p *PullRequest) Title() string                     { return p.title }
func (p *PullRequest) Description() string               { return p.description }
func (p *PullRequest) Author() shared.Author             { return p.author }
func (p *PullRequest) SourceBranch() shared.Branch       { return p.sourceBranch }
func (p *PullRequest) TargetBranch() shared.Branch       { return p.targetBranch }
func (p *PullRequest) State() State                      { return p.state }
func (p *PullRequest) Labels() []string                  { return append([]string(nil), p.labels...) }
func (p *PullRequest) CreatedAt() time.Time              { return p.createdAt }
func (p *PullRequest) UpdatedAt() time.Time              { return p.updatedAt }
func (p *PullRequest) MergedAt() *time.Time              { return p.mergedAt }
func (p *PullRequest) ClosedAt() *time.Time              { return p.closedAt }
func (p *PullRequest) MergeSHA() *shared.SHA             { return p.mergeSHA }
func (p *PullRequest) Files() []ChangedFile              { return append([]ChangedFile(nil), p.files...) }
func (p *PullRequest) Commits() []Commit                 { return append([]Commit(nil), p.commits...) }
func (p *PullRequest) Reviewers() []Reviewer             { return append([]Reviewer(nil), p.reviewers...) }
func (p *PullRequest) Tickets() []shared.TicketKey {
	return append([]shared.TicketKey(nil), p.tickets...)
}
func (p *PullRequest) PullEvents() []shared.DomainEvent { return p.emitter.PullEvents() }

// IsMerged returns true when the PR is in the Merged terminal state.
func (p *PullRequest) IsMerged() bool { return p.state == StateMerged }

// LinesChanged returns total additions+deletions across all files.
func (p *PullRequest) LinesChanged() int {
	var total int
	for _, f := range p.files {
		total += f.Additions + f.Deletions
	}
	return total
}

// ---------- Behavior ----------

// AttachFile adds a ChangedFile to the PR. Duplicate paths overwrite the
// existing entry (GitHub re-emits file lists per fetch).
func (p *PullRequest) AttachFile(f ChangedFile) error {
	if strings.TrimSpace(f.Path) == "" {
		return shared.NewValidationError("file.path", "must not be empty")
	}
	if !validFileStatus(f.Status) {
		return shared.NewValidationError("file.status", fmt.Sprintf("invalid status %q", f.Status))
	}
	for i := range p.files {
		if p.files[i].Path == f.Path {
			p.files[i] = f
			return nil
		}
	}
	p.files = append(p.files, f)
	return nil
}

// AttachCommit appends a commit. Duplicate SHAs are ignored.
func (p *PullRequest) AttachCommit(c Commit) error {
	if c.SHA.IsZero() {
		return shared.NewValidationError("commit.sha", "must be provided")
	}
	for _, existing := range p.commits {
		if existing.SHA.String() == c.SHA.String() {
			return nil
		}
	}
	p.commits = append(p.commits, c)
	return nil
}

// RecordReview adds or updates a reviewer entry.
func (p *PullRequest) RecordReview(r Reviewer) error {
	if r.Author.IsZero() {
		return shared.NewValidationError("reviewer.author", "must be provided")
	}
	if !validReviewState(r.State) {
		return shared.NewValidationError("reviewer.state", fmt.Sprintf("invalid state %q", r.State))
	}
	for i := range p.reviewers {
		if p.reviewers[i].Author.Login == r.Author.Login {
			p.reviewers[i] = r
			return nil
		}
	}
	p.reviewers = append(p.reviewers, r)
	return nil
}

// AssociateTicket links the PR to an external ticket (e.g. Jira issue).
// Duplicates are ignored.
func (p *PullRequest) AssociateTicket(t shared.TicketKey) {
	if t.IsZero() {
		return
	}
	for _, existing := range p.tickets {
		if existing.String() == t.String() {
			return
		}
	}
	p.tickets = append(p.tickets, t)
}

// Merge transitions the PR to the Merged state. Idempotent within the
// same SHA; rejects re-merge with a different SHA.
func (p *PullRequest) Merge(at time.Time, sha shared.SHA) error {
	if sha.IsZero() {
		return shared.NewValidationError("merge_sha", "must be provided")
	}
	if p.state == StateClosed {
		return fmt.Errorf("%w: cannot merge a closed PR", shared.ErrInvariant)
	}
	if p.state == StateMerged {
		if p.mergeSHA != nil && p.mergeSHA.String() == sha.String() {
			return nil // idempotent
		}
		return fmt.Errorf("%w: PR already merged with a different SHA", shared.ErrConflict)
	}
	p.state = StateMerged
	p.mergedAt = &at
	p.mergeSHA = &sha
	p.updatedAt = at
	p.emitter.Record(MergedEvent{
		BaseEvent:    shared.NewBaseEvent(p.id.String(), at),
		Number:       int(p.number),
		TargetBranch: p.targetBranch.String(),
		MergeSHA:     sha.String(),
	})
	return nil
}

// Close marks an unmerged PR as closed. No-op if already terminal.
func (p *PullRequest) Close(at time.Time) error {
	if p.state == StateMerged {
		return fmt.Errorf("%w: cannot close a merged PR", shared.ErrInvariant)
	}
	if p.state == StateClosed {
		return nil
	}
	p.state = StateClosed
	p.closedAt = &at
	p.updatedAt = at
	p.emitter.Record(ClosedEvent{
		BaseEvent: shared.NewBaseEvent(p.id.String(), at),
		Number:    int(p.number),
	})
	return nil
}

// ReplaceLabels overwrites the labels collection.
func (p *PullRequest) ReplaceLabels(labels []string, at time.Time) {
	p.labels = dedupeLabels(labels)
	p.updatedAt = at
}

// ---------- helpers ----------

func validFileStatus(s FileStatus) bool {
	switch s {
	case FileAdded, FileModified, FileRemoved, FileRenamed, FileCopied:
		return true
	}
	return false
}

func validReviewState(s ReviewState) bool {
	switch s {
	case ReviewRequested, ReviewApproved, ReviewChangesRequested, ReviewCommented, ReviewDismissed:
		return true
	}
	return false
}

func dedupeLabels(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, l := range in {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	return out
}

// ---------- Events ----------

// OpenedEvent fires when a new PR is opened in the system.
type OpenedEvent struct {
	shared.BaseEvent
	Number       int
	TargetBranch string
}

func (OpenedEvent) EventName() string { return "pull_request.opened" }

// MergedEvent fires when the PR transitions to Merged.
type MergedEvent struct {
	shared.BaseEvent
	Number       int
	TargetBranch string
	MergeSHA     string
}

func (MergedEvent) EventName() string { return "pull_request.merged" }

// ClosedEvent fires when the PR is closed without merge.
type ClosedEvent struct {
	shared.BaseEvent
	Number int
}

func (ClosedEvent) EventName() string { return "pull_request.closed" }
