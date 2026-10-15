package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/shared"
)

type pullRequestsRepo struct{ q dbtx }

var _ persistence.PullRequests = (*pullRequestsRepo)(nil)

// ---------- JSON DTOs for jsonb columns ----------

type fileDTO struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type authorDTO struct {
	Login string `json:"login"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

type commitDTO struct {
	SHA       string    `json:"sha"`
	Message   string    `json:"message"`
	Author    authorDTO `json:"author"`
	Committed time.Time `json:"committed"`
}

type reviewerDTO struct {
	Author authorDTO `json:"author"`
	State  string    `json:"state"`
	At     time.Time `json:"at"`
}

const prSelectCols = `
	id, repository_id, number, title, description, author_login, author_email,
	source_branch, target_branch, state, labels, tickets, files, commits, reviewers,
	created_at, updated_at, merged_at, closed_at, merge_sha
`

func (r *pullRequestsRepo) Save(ctx context.Context, pr *pullrequest.PullRequest) error {
	// Encode jsonb columns.
	labels := pr.Labels()
	if labels == nil {
		labels = []string{}
	}
	labelsB, err := json.Marshal(labels)
	if err != nil {
		return fmt.Errorf("postgres save pull_request: marshal labels: %w", err)
	}

	ticketsArr := make([]string, 0, len(pr.Tickets()))
	for _, t := range pr.Tickets() {
		ticketsArr = append(ticketsArr, t.String())
	}
	ticketsB, err := json.Marshal(ticketsArr)
	if err != nil {
		return fmt.Errorf("postgres save pull_request: marshal tickets: %w", err)
	}

	filesArr := make([]fileDTO, 0, len(pr.Files()))
	for _, f := range pr.Files() {
		filesArr = append(filesArr, fileDTO{
			Path:      f.Path,
			Status:    string(f.Status),
			Additions: f.Additions,
			Deletions: f.Deletions,
		})
	}
	filesB, err := json.Marshal(filesArr)
	if err != nil {
		return fmt.Errorf("postgres save pull_request: marshal files: %w", err)
	}

	commitsArr := make([]commitDTO, 0, len(pr.Commits()))
	for _, c := range pr.Commits() {
		commitsArr = append(commitsArr, commitDTO{
			SHA:       c.SHA.String(),
			Message:   c.Message,
			Author:    authorDTO{Login: c.Author.Login, Email: c.Author.Email, Name: c.Author.Name},
			Committed: c.Committed.UTC(),
		})
	}
	commitsB, err := json.Marshal(commitsArr)
	if err != nil {
		return fmt.Errorf("postgres save pull_request: marshal commits: %w", err)
	}

	reviewersArr := make([]reviewerDTO, 0, len(pr.Reviewers()))
	for _, rv := range pr.Reviewers() {
		reviewersArr = append(reviewersArr, reviewerDTO{
			Author: authorDTO{Login: rv.Author.Login, Email: rv.Author.Email, Name: rv.Author.Name},
			State:  string(rv.State),
			At:     rv.At.UTC(),
		})
	}
	reviewersB, err := json.Marshal(reviewersArr)
	if err != nil {
		return fmt.Errorf("postgres save pull_request: marshal reviewers: %w", err)
	}

	var mergeSHA *string
	if s := pr.MergeSHA(); s != nil {
		v := s.String()
		mergeSHA = &v
	}

	const q = `
INSERT INTO pull_request (
	id, repository_id, number, title, description, author_login, author_email,
	source_branch, target_branch, state, labels, tickets, files, commits, reviewers,
	created_at, updated_at, merged_at, closed_at, merge_sha
) VALUES (
	$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20
)
ON CONFLICT (repository_id, number) DO UPDATE SET
	title          = EXCLUDED.title,
	description    = EXCLUDED.description,
	author_login   = EXCLUDED.author_login,
	author_email   = EXCLUDED.author_email,
	source_branch  = EXCLUDED.source_branch,
	target_branch  = EXCLUDED.target_branch,
	state          = EXCLUDED.state,
	labels         = EXCLUDED.labels,
	tickets        = EXCLUDED.tickets,
	files          = EXCLUDED.files,
	commits        = EXCLUDED.commits,
	reviewers      = EXCLUDED.reviewers,
	updated_at     = EXCLUDED.updated_at,
	merged_at      = EXCLUDED.merged_at,
	closed_at      = EXCLUDED.closed_at,
	merge_sha      = EXCLUDED.merge_sha`

	_, err = r.q.Exec(ctx, q,
		pr.ID().UUID(),
		pr.RepositoryID().UUID(),
		int(pr.Number()),
		pr.Title(),
		pr.Description(),
		pr.Author().Login,
		pr.Author().Email,
		pr.SourceBranch().String(),
		pr.TargetBranch().String(),
		string(pr.State()),
		labelsB,
		ticketsB,
		filesB,
		commitsB,
		reviewersB,
		pr.CreatedAt(),
		pr.UpdatedAt(),
		pr.MergedAt(),
		pr.ClosedAt(),
		mergeSHA,
	)
	return classify("save pull_request", err)
}

func (r *pullRequestsRepo) FindByID(ctx context.Context, id shared.PullRequestID) (*pullrequest.PullRequest, error) {
	return r.findOne(ctx, "WHERE id = $1", id.UUID())
}

func (r *pullRequestsRepo) FindByRepoNumber(
	ctx context.Context,
	repoID shared.RepositoryID,
	number pullrequest.Number,
) (*pullrequest.PullRequest, error) {
	return r.findOne(ctx, "WHERE repository_id = $1 AND number = $2", repoID.UUID(), int(number))
}

func (r *pullRequestsRepo) ListByTickets(
	ctx context.Context,
	tickets []shared.TicketKey,
) ([]*pullrequest.PullRequest, error) {
	if len(tickets) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(tickets))
	for _, t := range tickets {
		if !t.IsZero() {
			keys = append(keys, t.String())
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(keys)
	if err != nil {
		return nil, fmt.Errorf("postgres list pull_requests: marshal tickets: %w", err)
	}
	// `tickets ?| $1` would use a text[] of keys; we use jsonb @> ANY pattern
	// via a single query: pull_request.tickets ?| (SELECT array_agg(value)
	// FROM jsonb_array_elements_text($1::jsonb)).
	q := "SELECT " + prSelectCols + ` FROM pull_request
WHERE tickets ?| (
	SELECT array_agg(value)::text[] FROM jsonb_array_elements_text($1::jsonb)
)
ORDER BY merged_at NULLS LAST, created_at DESC`

	rows, err := r.q.Query(ctx, q, payload)
	if err != nil {
		return nil, classify("list pull_requests by tickets", err)
	}
	defer rows.Close()

	var out []*pullrequest.PullRequest
	for rows.Next() {
		pr, err := scanPullRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	if err := rows.Err(); err != nil {
		return nil, classify("list pull_requests by tickets: rows", err)
	}
	return out, nil
}

// ListMergedWithoutTicketsInWindow implements the port. Backs the
// release-notes "Unlinked PRs" section: PRs merged during the sprint
// window whose ticket extraction (title / body / branch / commit
// messages) came up empty.
func (r *pullRequestsRepo) ListMergedWithoutTicketsInWindow(
	ctx context.Context,
	start, end time.Time,
) ([]*pullrequest.PullRequest, error) {
	// A zero / inverted window returns no rows -- callers commonly
	// pass sprint.StartsAt() / sprint.EndsAt() which may be nil for
	// unbounded sprints. Keeping this pre-check makes the release-
	// notes handler's happy path branchless.
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return nil, nil
	}
	q := "SELECT " + prSelectCols + ` FROM pull_request
WHERE merged_at IS NOT NULL
  AND merged_at BETWEEN $1 AND $2
  AND (tickets IS NULL OR jsonb_array_length(tickets) = 0)
ORDER BY merged_at DESC`
	rows, err := r.q.Query(ctx, q, start, end)
	if err != nil {
		return nil, classify("list pull_requests without tickets", err)
	}
	defer rows.Close()
	var out []*pullrequest.PullRequest
	for rows.Next() {
		pr, err := scanPullRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	if err := rows.Err(); err != nil {
		return nil, classify("list pull_requests without tickets: rows", err)
	}
	return out, nil
}

func (r *pullRequestsRepo) findOne(
	ctx context.Context,
	where string,
	args ...any,
) (*pullrequest.PullRequest, error) {
	rows, err := r.q.Query(ctx, "SELECT "+prSelectCols+" FROM pull_request "+where+" LIMIT 1", args...)
	if err != nil {
		return nil, classify("find pull_request", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, classify("find pull_request: rows", err)
		}
		// Must pass pgx.ErrNoRows (not a fresh errors.New) so classify
		// maps to persistence.ErrNotFound. A raw error would fall
		// through the classify switch and break errors.Is checks in
		// PRIngestor / release-notes handler.
		return nil, classify("find pull_request", pgx.ErrNoRows)
	}
	pr, err := scanPullRequest(rows)
	if err != nil {
		return nil, err
	}
	return pr, nil
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanPullRequest(rs rowScanner) (*pullrequest.PullRequest, error) {
	var (
		rawID        uuid.UUID
		rawRepoID    uuid.UUID
		number       int
		title        string
		description  string
		authorLogin  string
		authorEmail  string
		sourceBranch string
		targetBranch string
		state        string
		labelsB      []byte
		ticketsB     []byte
		filesB       []byte
		commitsB     []byte
		reviewersB   []byte
		createdAt    time.Time
		updatedAt    time.Time
		mergedAt     *time.Time
		closedAt     *time.Time
		mergeSHARaw  *string
	)
	if err := rs.Scan(
		&rawID, &rawRepoID, &number, &title, &description, &authorLogin, &authorEmail,
		&sourceBranch, &targetBranch, &state,
		&labelsB, &ticketsB, &filesB, &commitsB, &reviewersB,
		&createdAt, &updatedAt, &mergedAt, &closedAt, &mergeSHARaw,
	); err != nil {
		return nil, classify("scan pull_request", err)
	}

	id, err := shared.ParsePullRequestID(rawID.String())
	if err != nil {
		return nil, classify("scan pull_request: corrupt id", err)
	}
	repoID, err := shared.ParseRepositoryID(rawRepoID.String())
	if err != nil {
		return nil, classify("scan pull_request: corrupt repo id", err)
	}
	source, err := shared.NewBranch(sourceBranch)
	if err != nil {
		return nil, classify("scan pull_request: corrupt source branch", err)
	}
	target, err := shared.NewBranch(targetBranch)
	if err != nil {
		return nil, classify("scan pull_request: corrupt target branch", err)
	}
	author, err := shared.NewAuthor(authorLogin, "", authorEmail)
	if err != nil {
		return nil, classify("scan pull_request: corrupt author", err)
	}

	st := pullrequest.State(state)
	if !st.Valid() {
		return nil, classify("scan pull_request: corrupt state",
			errors.New("invalid state "+state))
	}

	var labels []string
	if err := unmarshalNonNull(labelsB, &labels); err != nil {
		return nil, classify("scan pull_request: labels", err)
	}

	var tkRaw []string
	if err := unmarshalNonNull(ticketsB, &tkRaw); err != nil {
		return nil, classify("scan pull_request: tickets", err)
	}
	tickets := make([]shared.TicketKey, 0, len(tkRaw))
	for _, s := range tkRaw {
		tk, err := shared.NewTicketKey(s)
		if err != nil {
			return nil, classify("scan pull_request: ticket key "+s, err)
		}
		tickets = append(tickets, tk)
	}

	var filesArr []fileDTO
	if err := unmarshalNonNull(filesB, &filesArr); err != nil {
		return nil, classify("scan pull_request: files", err)
	}
	files := make([]pullrequest.ChangedFile, 0, len(filesArr))
	for _, f := range filesArr {
		files = append(files, pullrequest.ChangedFile{
			Path:      f.Path,
			Status:    pullrequest.FileStatus(f.Status),
			Additions: f.Additions,
			Deletions: f.Deletions,
		})
	}

	var commitsArr []commitDTO
	if err := unmarshalNonNull(commitsB, &commitsArr); err != nil {
		return nil, classify("scan pull_request: commits", err)
	}
	commits := make([]pullrequest.Commit, 0, len(commitsArr))
	for _, c := range commitsArr {
		sha, err := shared.NewSHA(c.SHA)
		if err != nil {
			return nil, classify("scan pull_request: commit sha "+c.SHA, err)
		}
		cAuthor, err := shared.NewAuthor(c.Author.Login, c.Author.Name, c.Author.Email)
		if err != nil {
			return nil, classify("scan pull_request: commit author", err)
		}
		commits = append(commits, pullrequest.Commit{
			SHA:       sha,
			Message:   c.Message,
			Author:    cAuthor,
			Committed: c.Committed.UTC(),
		})
	}

	var reviewersArr []reviewerDTO
	if err := unmarshalNonNull(reviewersB, &reviewersArr); err != nil {
		return nil, classify("scan pull_request: reviewers", err)
	}
	reviewers := make([]pullrequest.Reviewer, 0, len(reviewersArr))
	for _, rv := range reviewersArr {
		rvAuthor, err := shared.NewAuthor(rv.Author.Login, rv.Author.Name, rv.Author.Email)
		if err != nil {
			return nil, classify("scan pull_request: reviewer author", err)
		}
		reviewers = append(reviewers, pullrequest.Reviewer{
			Author: rvAuthor,
			State:  pullrequest.ReviewState(rv.State),
			At:     rv.At.UTC(),
		})
	}

	var mergeSHA *shared.SHA
	if mergeSHARaw != nil {
		sha, err := shared.NewSHA(*mergeSHARaw)
		if err != nil {
			return nil, classify("scan pull_request: corrupt merge sha", err)
		}
		mergeSHA = &sha
	}

	return pullrequest.Rehydrate(
		id, repoID, pullrequest.Number(number),
		title, description, author, source, target, st,
		labels,
		createdAt.UTC(), updatedAt.UTC(),
		ptrUTC(mergedAt), ptrUTC(closedAt),
		mergeSHA,
		files, commits, reviewers, tickets,
	), nil
}

// unmarshalNonNull decodes JSON bytes into v, treating nil/empty bytes
// as a no-op (leaves v zero-valued).
func unmarshalNonNull(b []byte, v any) error {
	if len(b) == 0 || strings.EqualFold(string(b), "null") {
		return nil
	}
	return json.Unmarshal(b, v)
}
