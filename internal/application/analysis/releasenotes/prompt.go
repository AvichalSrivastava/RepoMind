package releasenotes

import (
	"fmt"
	"strings"
	"time"

	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/sprint"
)

// systemPrompt instructs the model on its role and output contract.
// The wire-level JSON Schema is enforced by the provider (strict
// mode); this prompt provides the semantic guidance the schema can't
// express.
const systemPrompt = `You are a release-notes composer for an engineering team's sprint.

Given a sprint header (name, dates, goal) and a list of merged pull requests, return a strict JSON object that matches the schema you have been given. Your job is to:

- Write a short Summary (1-3 sentences) of what shipped this sprint, suitable as a Confluence intro paragraph.
- Produce a rendered Markdown body grouping changes by category. Use H3 headings per category in this canonical order: Breaking changes, Security, Features, Improvements, Bug fixes, Chores. Omit empty sections. Each bullet should be a single line that cites the PR title and links any ticket key(s) when present.
- Emit one Entry per change worth highlighting, with:
  - Category: one of "feature", "improvement", "bugfix", "security", "breaking_change", "chore".
  - Title: short imperative phrase capturing the PR's intent (not the verbatim PR title).
  - Description: optional one-paragraph elaboration; "" if the title is self-explanatory.
  - PR: the RepoMind PullRequestID exactly as given to you (UUID string, do not alter or guess).
  - Tickets: the Jira ticket key(s) associated with the PR; empty array if none.

Do not include any prose outside the JSON object. Do not fabricate PRs or tickets that weren't in the input. Always emit at least one Entry.`

// Caps keep the prompt bounded so a pathological sprint (hundreds of
// PRs) can't blow our token budget. Promote to Config if a real sprint
// exceeds them.
const (
	maxPRsListed          = 100
	maxPRDescriptionRunes = 300
	maxTicketsListedPerPR = 10
)

// buildUserPrompt renders a deterministic, structured sprint summary
// for the model. Field order is fixed and lists are truncated in their
// natural order so prompt diffs across two analyzer runs of the same
// sprint are minimal and reviewable.
func buildUserPrompt(s *sprint.Sprint, prs []*pullrequest.PullRequest) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Sprint: %s\n", s.Name())
	fmt.Fprintf(&b, "Team: %s\n", s.Team())
	fmt.Fprintf(&b, "State: %s\n", s.State())
	if g := strings.TrimSpace(s.Goal()); g != "" {
		fmt.Fprintf(&b, "Goal: %s\n", g)
	}
	fmt.Fprintf(&b, "Dates: %s -> %s\n", fmtDate(s.StartsAt()), fmtDate(s.EndsAt()))

	shown := minInt(len(prs), maxPRsListed)
	fmt.Fprintf(&b, "\nPull requests (%d total, showing %d):\n", len(prs), shown)

	for i, pr := range prs {
		if i >= maxPRsListed {
			break
		}
		fmt.Fprintf(&b, "\n[%d] PR #%d %s\n", i+1, pr.Number(), pr.ID().String())
		fmt.Fprintf(&b, "    Title: %s\n", pr.Title())
		fmt.Fprintf(&b, "    Author: %s\n", pr.Author().Login)
		fmt.Fprintf(&b, "    Target: %s\n", pr.TargetBranch())
		fmt.Fprintf(&b, "    State: %s\n", pr.State())
		fmt.Fprintf(&b, "    Lines changed: %d\n", pr.LinesChanged())
		fmt.Fprintf(&b, "    Files: %d changed\n", len(pr.Files()))

		if labels := pr.Labels(); len(labels) > 0 {
			fmt.Fprintf(&b, "    Labels: %s\n", strings.Join(labels, ", "))
		}

		if tickets := pr.Tickets(); len(tickets) > 0 {
			limit := minInt(len(tickets), maxTicketsListedPerPR)
			keys := make([]string, 0, limit)
			for j, t := range tickets {
				if j >= maxTicketsListedPerPR {
					break
				}
				keys = append(keys, t.String())
			}
			fmt.Fprintf(&b, "    Tickets: %s\n", strings.Join(keys, ", "))
		}

		if desc := truncateRunes(pr.Description(), maxPRDescriptionRunes); desc != "" {
			fmt.Fprintf(&b, "    Description: %s\n", singleLine(desc))
		}
	}

	return b.String()
}

func fmtDate(t *time.Time) string {
	if t == nil {
		return "(unknown)"
	}
	return t.UTC().Format("2006-01-02")
}

func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// singleLine collapses any embedded newlines so a multi-paragraph PR
// description doesn't fragment the per-PR block in the prompt.
func singleLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
