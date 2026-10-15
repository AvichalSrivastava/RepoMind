package orchestration

import (
	"regexp"
	"strings"

	"github.com/repomind/repomind/internal/domain/shared"
)

// ticketKeyScan matches Jira-shaped keys anywhere in a text with word
// boundaries so we don't glue onto surrounding words. The character
// class mirrors shared.ticketKeyPattern (project prefix rules) so any
// match is guaranteed to parse via shared.NewTicketKey.
//
// Boundary reasoning:
//   - `\b` at each end prevents "someSIS-9505" (leading letters) or
//     "SIS-9505999" collision with adjacent word chars.
//   - `(?i)` accepts lowercase inputs (e.g. head branch
//     `feature/sis-9505`), which NewTicketKey then uppercases.
//   - Underscore is a word char in Go regexp, so `MY_KEY-100` still
//     matches at a `/` or space boundary.
var ticketKeyScan = regexp.MustCompile(`(?i)\b[A-Z][A-Z0-9_]{1,15}-[0-9]{1,10}\b`)

// extractTicketKeys scans the given free-text sources for Jira-shaped
// ticket keys, validates each via shared.NewTicketKey, deduplicates
// while preserving first-seen order, and returns the canonical
// (uppercased) keys.
//
// Sources: any mix of PR title, PR body, head branch name, commit
// messages, etc. Empty strings and duplicates are safe.
//
// Corner cases handled:
//   - Case insensitive: "sis-9505" -> SIS-9505.
//   - Multiple occurrences dedup: "SIS-1 and SIS-1" -> [SIS-1].
//   - Path segment: "feature/SIS-9505-report-fix" -> SIS-9505 (the
//     "-report" is not [0-9] so the number ends at "9505").
//   - URL: ".../browse/SIS-9505" -> SIS-9505.
//   - Trailing letters glued to a key: "SIS-9505abc" -> NO match
//     because `\b` requires a non-word transition after the digits.
//   - Nil/empty inputs: returns nil (never panics).
//
// Known permissive behaviour:
//   - "someSIS-9505" matches as SOMESIS-9505: without a full Jira
//     project list at hand the extractor cannot distinguish accidental
//     prefixes from real ones. Downstream, release_notes_handler
//     joins PR tickets against the sprint's actual issue keys via
//     PullRequests.ListByTickets, so unknown-project matches are
//     silently filtered out and never surface on Confluence.
//   - "SIS-9505999" matches as SIS-9505999 (greedy digits). Same
//     downstream filter neutralises this.
func extractTicketKeys(sources ...string) []shared.TicketKey {
	if len(sources) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []shared.TicketKey
	for _, src := range sources {
		if src == "" {
			continue
		}
		for _, raw := range ticketKeyScan.FindAllString(src, -1) {
			canonical := strings.ToUpper(raw)
			if _, dup := seen[canonical]; dup {
				continue
			}
			tk, err := shared.NewTicketKey(canonical)
			if err != nil {
				// Should not happen given ticketKeyScan mirrors the
				// domain regex, but be defensive: skip silently.
				continue
			}
			seen[canonical] = struct{}{}
			out = append(out, tk)
		}
	}
	return out
}
