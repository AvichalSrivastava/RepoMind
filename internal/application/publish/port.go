// Package publish defines the application-layer port for pushing
// generated artefacts (release notes today, possibly assessment
// summaries tomorrow) to a downstream document store such as
// Confluence, Notion, or a Slack channel.
//
// Domain types flow in, vendor-specific identifiers (page id, section
// name) flow in as opaque strings. The infrastructure adapter
// implements this port; the orchestration use cases in Modules 9/10
// consume it.
//
// Keeping the port here — rather than letting the use case import
// internal/infrastructure/confluence directly — preserves the
// Clean-Architecture dependency rule: application code never imports
// infrastructure.
package publish

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/repomind/repomind/internal/domain/analysis"
)

// Publisher pushes a rendered release-notes payload to a downstream
// document store. Implementations must be safe for concurrent use.
type Publisher interface {
	Publish(ctx context.Context, in Input) (*Result, error)
}

// Input bundles everything the publisher needs to update a single
// named section on a single target page.
//
// SectionName is an opaque marker key, not a heading. Two sprints
// published to the same page must use different SectionName values so
// their content does not overwrite each other; the publisher uses the
// name to locate the existing block (if any) between matching
// `<!-- repomind:section:start name="..." -->` / `:end` markers.
//
// Heading is the human-readable H2 that the publisher renders at the
// top of the section body (e.g. "Marvelous Sprint 42 — closed
// 2026-06-28"). Notes carries the structured release-notes payload
// produced by the M7 analyzer; the publisher renders Notes.Markdown
// into the document store's native body format.
type Input struct {
	PageID      string
	SectionName string
	Heading     string
	Notes       *analysis.ReleaseNotes
	GeneratedAt time.Time

	// LogRows, when non-empty, instructs the publisher to splice each
	// row into the release-log table on the same page in the same PUT.
	// Each row corresponds to ONE Jira ticket; the publisher's
	// append-only logic uses (SprintID, TicketKey) as the row anchor
	// so operator hand-edits to a specific ticket row survive
	// subsequent publishes.
	//
	// The narrative section (SectionName / Heading / Notes) is
	// independent and always replace-on-publish.
	LogRows []ReleaseLogRow

	// ForceRefresh, when true, instructs the publisher to REPLACE
	// existing (SprintID, TicketKey) rows in the release-log table
	// with the freshly-built values, instead of skipping them. Use
	// this to backfill a column (e.g. QA Owner) added after the
	// original publish. Operator hand-edits to affected rows will
	// be overwritten -- callers should reserve this for explicit
	// operator-invoked refreshes (e.g. ?force=true on the API).
	ForceRefresh bool

	// UnlinkedPRs carries merged PRs that could not be attributed
	// to any Jira ticket during ingestion (title, body, branch,
	// commit messages all had no ticket key). The publisher renders
	// them into a small "Unlinked PRs" section on the same page in
	// the same PUT. Semantics differ from LogRows: this section is
	// AUTHORITATIVE per publish -- the publisher replaces the
	// section body with these rows every time, so PRs that get a
	// ticket added later (author amends the PR title, next publish)
	// naturally drop out of the section.
	//
	// Empty slice + no existing section on page: nothing rendered.
	// Empty slice + existing section on page: section body cleared.
	UnlinkedPRs []UnlinkedPRRef
}

// ReleaseLogRow is one ticket-scoped row in the release log table.
// The table has fixed columns matching the operator's preferred
// layout:
//
//	| Release Planned Date | Sprint Start | Sprint ID | Ticket ID |
//	  Description | Dev Owner | QA Owner | Release Details | Sprint End |
//
// RepoMind owns every column except Release Planned Date, which is
// operator-fillable -- the row's anchor preserves that edit across
// re-publishes. QA Owner is auto-populated from the Jira "QA Owner"
// custom field (see Jira.QAOwnerFieldID); Dev Owner is derived from
// PR authors, falling back to the earliest non-QA Jira commenter.
type ReleaseLogRow struct {
	SprintID        uuid.UUID
	SprintJiraID    int64     // internal Jira sprint id (e.g. 3974)
	SprintNumber    int       // short, human-friendly sprint number (e.g. 185)
	SprintStartDate time.Time // sprint.StartsAt
	SprintEndDate   time.Time // sprint.EndsAt
	ReleaseDate     time.Time // planned release date; defaults to SprintEndDate
	TicketKey       string    // canonical Jira key, e.g. "SIS-9506"
	Description     string    // Jira summary (optional)
	JiraBaseURL     string    // optional; ticket renders as link when set
	DevOwners       []string  // PR author logins, or backtracked from Jira comments
	QAOwners        []string  // from Jira "QA Owner" custom field (single-element slice today)
	PRs             []PRRef   // PRs that touched this ticket, sorted by repo
}

// PRRef is the minimal projection of a pull-request needed to render
// a clickable entry in the Release Steps/Details cell.
type PRRef struct {
	RepoFullName string // "SHL-India/MFS-mfs-api"
	Number       int
	Title        string
	URL          string // PR's html_url; must be http(s); empty -> not linked
}

// UnlinkedPRRef is a merged PR that could not be attributed to any
// Jira ticket during ingestion (title, body, branch, commit messages
// all had no ticket key). Rendered as a small "Unlinked PRs" table
// below the release-log table so operators can eyeball them and
// either amend the PR title (then re-publish) or manually attach a
// ticket.
type UnlinkedPRRef struct {
	RepoFullName string
	Number       int
	Title        string
	Author       string
	HeadBranch   string
	MergedAt     time.Time
	URL          string // PR html_url; must be http(s); empty -> not linked
}

// Result captures what the publisher actually wrote, so callers can
// log / persist provenance for audit.
type Result struct {
	PageID     string
	NewVersion int
	URL        string
}

// Sentinel errors. Callers should discriminate via errors.Is rather
// than string-matching, and never surface raw response bodies (PII).
var (
	ErrUnauthorized = errors.New("publish: unauthorized")
	ErrForbidden    = errors.New("publish: forbidden")
	ErrNotFound     = errors.New("publish: page not found")
	ErrRateLimited  = errors.New("publish: rate limited")
	ErrServer       = errors.New("publish: server error")
	ErrConflict     = errors.New("publish: version conflict")
	ErrInvalidPage  = errors.New("publish: page body is in an unexpected shape")
)
