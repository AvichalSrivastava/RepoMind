package analysis

import (
	"strings"

	"github.com/repomind/repomind/internal/domain/shared"
)

// ChangeCategory groups PRs in release notes.
type ChangeCategory string

const (
	CategoryFeature        ChangeCategory = "feature"
	CategoryImprovement    ChangeCategory = "improvement"
	CategoryBugfix         ChangeCategory = "bugfix"
	CategorySecurity       ChangeCategory = "security"
	CategoryBreakingChange ChangeCategory = "breaking_change"
	CategoryChore          ChangeCategory = "chore"
)

func (c ChangeCategory) Valid() bool {
	switch c {
	case CategoryFeature, CategoryImprovement, CategoryBugfix,
		CategorySecurity, CategoryBreakingChange, CategoryChore:
		return true
	}
	return false
}

// ReleaseNoteEntry is one bullet in the release notes.
type ReleaseNoteEntry struct {
	Category    ChangeCategory
	Title       string
	Description string
	PR          shared.PullRequestID
	Tickets     []shared.TicketKey
}

// ReleaseNotes is the Payload produced by ReleaseNotesAgent.
//
// The aggregate keeps both a structured form (entries grouped by
// category) and a rendered markdown form (suitable for Confluence /
// Slack / email). Producing the markdown is the agent's responsibility,
// not the domain's — the domain only validates and stores.
type ReleaseNotes struct {
	Summary  string // one-paragraph summary
	Markdown string // pre-rendered markdown
	Entries  []ReleaseNoteEntry
}

// NewReleaseNotes validates and constructs a ReleaseNotes payload.
func NewReleaseNotes(summary, markdown string, entries []ReleaseNoteEntry) (ReleaseNotes, error) {
	if strings.TrimSpace(summary) == "" {
		return ReleaseNotes{}, shared.NewValidationError("summary", "must not be empty")
	}
	if strings.TrimSpace(markdown) == "" {
		return ReleaseNotes{}, shared.NewValidationError("markdown", "must not be empty")
	}
	if len(entries) == 0 {
		return ReleaseNotes{}, shared.NewValidationError("entries", "must contain at least one entry")
	}
	for i, e := range entries {
		if !e.Category.Valid() {
			return ReleaseNotes{}, shared.NewValidationError("entries.category", "invalid category at index "+itoa(i))
		}
		if strings.TrimSpace(e.Title) == "" {
			return ReleaseNotes{}, shared.NewValidationError("entries.title", "must not be empty at index "+itoa(i))
		}
		if e.PR.IsZero() {
			return ReleaseNotes{}, shared.NewValidationError("entries.pr", "must reference a PR at index "+itoa(i))
		}
	}
	return ReleaseNotes{Summary: strings.TrimSpace(summary), Markdown: markdown, Entries: entries}, nil
}

// payloadKind implements analysis.Payload.
func (ReleaseNotes) payloadKind() Kind { return KindReleaseNotes }

// GroupByCategory returns entries bucketed by ChangeCategory in the
// canonical display order.
func (rn ReleaseNotes) GroupByCategory() map[ChangeCategory][]ReleaseNoteEntry {
	out := make(map[ChangeCategory][]ReleaseNoteEntry, 6)
	for _, e := range rn.Entries {
		out[e.Category] = append(out[e.Category], e)
	}
	return out
}

// itoa avoids importing strconv into a hot validator path.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
