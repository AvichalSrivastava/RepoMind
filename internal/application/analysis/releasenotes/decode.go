package releasenotes

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/shared"
)

// schemaPayload mirrors the on-the-wire JSON shape declared by
// internal/infrastructure/openai/schemas/release_notes.json.
//
// We round-trip through this DTO rather than unmarshalling directly
// into analysis.ReleaseNotes so the type conversions (PR string ->
// shared.PullRequestID, ticket strings -> []shared.TicketKey) stay
// explicit and the domain constructor — not json.Unmarshal — owns
// invariant enforcement.
type schemaPayload struct {
	Summary  string        `json:"Summary"`
	Markdown string        `json:"Markdown"`
	Entries  []schemaEntry `json:"Entries"`
}

// schemaEntry mirrors the Entries[] item shape.
type schemaEntry struct {
	Category    string   `json:"Category"`
	Title       string   `json:"Title"`
	Description string   `json:"Description"`
	PR          string   `json:"PR"`      // UUID
	Tickets     []string `json:"Tickets"` // e.g. ["SIS-100", "SIS-101"]
}

// decodePayload converts the model's JSON response into a validated
// domain ReleaseNotes. Both JSON-shape, ID-parsing, and
// domain-invariant failures surface here and are wrapped with
// llm.ErrInvalidResponse by the caller in analyzer.go.
//
// As a small reliability hedge against models that ignore strict mode
// and wrap their JSON in a markdown code fence, we strip a
// leading/trailing ```...``` block before unmarshalling.
//
// We validate PR ids and ticket keys at decode time (not just in the
// domain constructor) because the model can hallucinate UUIDs or
// malformed ticket strings even when strict-mode JSON validation is
// active — the schema only checks `string`, not "valid UUID" or
// "matches PROJ-N".
func decodePayload(raw []byte) (analysis.ReleaseNotes, error) {
	raw = []byte(stripCodeFence(string(raw)))

	var p schemaPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return analysis.ReleaseNotes{}, fmt.Errorf("unmarshal: %w", err)
	}

	entries := make([]analysis.ReleaseNoteEntry, 0, len(p.Entries))
	for i, e := range p.Entries {
		prID, err := shared.ParsePullRequestID(e.PR)
		if err != nil {
			return analysis.ReleaseNotes{}, fmt.Errorf("entries[%d].PR: %w", i, err)
		}
		tickets := make([]shared.TicketKey, 0, len(e.Tickets))
		for j, t := range e.Tickets {
			tk, err := shared.NewTicketKey(t)
			if err != nil {
				return analysis.ReleaseNotes{}, fmt.Errorf("entries[%d].Tickets[%d]: %w", i, j, err)
			}
			tickets = append(tickets, tk)
		}
		entries = append(entries, analysis.ReleaseNoteEntry{
			Category:    analysis.ChangeCategory(e.Category),
			Title:       e.Title,
			Description: e.Description,
			PR:          prID,
			Tickets:     tickets,
		})
	}

	return analysis.NewReleaseNotes(p.Summary, p.Markdown, entries)
}

// stripCodeFence removes a single surrounding markdown code fence if
// present. Handles plain ``` and language-tagged ```json. Idempotent
// and safe on non-fenced input (returns the trimmed string).
//
// Duplicated from the risk / qa analyzers — extracting to a shared
// internal helper package becomes worthwhile once we need to evolve
// the fence-handling logic, not on first reuse.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimRight(s, " \t\r\n")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
