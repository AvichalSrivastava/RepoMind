// Package jira defines the application-layer port used to read sprint
// and issue data from Jira. The HTTP adapter lives in
// internal/infrastructure/jira.
//
// In V1, Jira is the authoritative source of truth for sprints. This
// port is the only seam between RepoMind and Jira; nothing else in the
// codebase imports Jira-specific types.
package jira

import (
	"context"
	"time"
)

// Client is the port through which the application layer queries Jira.
// Implementations MUST be safe for concurrent use.
type Client interface {
	// ListBoards returns boards visible to the configured credentials.
	ListBoards(ctx context.Context, opts ListBoardsOptions) (*BoardPage, error)

	// ListSprints returns sprints under a board, filterable by state.
	ListSprints(ctx context.Context, boardID int64, opts ListSprintsOptions) (*SprintPage, error)

	// GetSprint returns a single sprint by its Jira id.
	GetSprint(ctx context.Context, sprintID int64) (*Sprint, error)

	// ListSprintIssues returns the issues currently in a sprint, with
	// optional JQL refinement and a field whitelist.
	ListSprintIssues(ctx context.Context, sprintID int64, opts ListSprintIssuesOptions) (*IssuePage, error)
}

// ---------- Pagination & filter options ----------

// PageOptions is the cursor-based pagination input. The cursor is an
// opaque token returned by the previous page; pass empty for the first
// page. Limit ≤ 0 means use the server default.
type PageOptions struct {
	Cursor string
	Limit  int
}

// ListBoardsOptions filters the board list.
type ListBoardsOptions struct {
	Page          PageOptions
	ProjectKey    string // Jira project key (optional)
	BoardType     string // "scrum" | "kanban" | "" (any)
	NameSubstring string // case-insensitive substring filter (optional)
}

// ListSprintsOptions filters sprints under a board.
type ListSprintsOptions struct {
	Page  PageOptions
	State string // e.g. "active" | "closed" | "future" | "active,closed"
}

// ListSprintIssuesOptions filters issues within a sprint.
type ListSprintIssuesOptions struct {
	Page   PageOptions
	JQL    string   // additional JQL clause, AND'd with sprint filter
	Fields []string // server-side field projection
}

// ---------- DTOs returned by the port ----------

// Board is a Jira agile board.
type Board struct {
	ID         int64
	Name       string
	Type       string
	ProjectKey string
}

// Sprint is a Jira sprint as observed at the time of the call.
type Sprint struct {
	ID            int64
	Name          string
	State         string // "future" | "active" | "closed"
	StartDate     *time.Time
	EndDate       *time.Time
	CompleteDate  *time.Time
	OriginBoardID int64
	Goal          string
}

// Issue is a Jira issue as observed in a sprint.
type Issue struct {
	ID      string
	Key     string
	Summary string
	Status  string
	Type    string
	// Assignee is the display name of the CURRENT Jira assignee.
	// Note: this project follows a workflow where the assignee is
	// flipped to the QA owner after dev-complete, so the current
	// value should NOT be trusted as the dev owner. Prefer PR
	// authors, then Comments-derived dev owner, then this field
	// only as a last-resort fallback (with a QAOwner guard).
	Assignee string
	// QAOwner is the display name of the person carried in the
	// "QA Owner" Jira custom field (id configured via
	// REPOMIND_JIRA_QA_OWNER_FIELD_ID). Empty when the field is
	// unset on the issue or the field id is not configured.
	QAOwner string
	// Reporter is the display name of the ticket reporter. Used as
	// the QA Owner fallback when the QA Owner custom field is
	// unset -- reporters are frequently QA leads or product folks
	// in this project's workflow.
	Reporter   string
	Labels     []string
	Components []string
	// Comments are the ticket's comments in chronological order
	// (oldest first). Used by the release-notes handler to
	// backtrack the dev owner (earliest commenter who is not the
	// QA owner).
	Comments []IssueComment
	// StatusTransitions is the ticket's status change history in
	// chronological order (oldest first), derived from the Jira
	// changelog. Used by the release-notes handler to identify the
	// dev owner as the author of the last transition into an
	// "In Development"-like status -- a much stronger signal than
	// comment authorship.
	StatusTransitions []IssueStatusTransition
}

// IssueComment is a minimal projection of a Jira comment: just
// enough to backtrack the dev owner from the earliest non-QA
// commenter. Body is intentionally omitted -- we don't render
// comment bodies and adding them here would balloon page sizes.
type IssueComment struct {
	Author    string    // display name (falls back to email)
	CreatedAt time.Time // UTC
}

// IssueStatusTransition is a single status change on a Jira issue,
// derived from the changelog. From/To carry the human-readable
// status names ("Backlog" / "In Development" / etc.).
type IssueStatusTransition struct {
	Author    string    // display name (falls back to email)
	From      string    // previous status name (may be empty)
	To        string    // new status name
	CreatedAt time.Time // UTC
}

// ---------- Page envelopes ----------

// BoardPage is a paginated Board response.
type BoardPage struct {
	Items      []Board
	NextCursor string // empty when there are no further pages
}

// SprintPage is a paginated Sprint response.
type SprintPage struct {
	Items      []Sprint
	NextCursor string
}

// IssuePage is a paginated Issue response.
type IssuePage struct {
	Items      []Issue
	NextCursor string
}
