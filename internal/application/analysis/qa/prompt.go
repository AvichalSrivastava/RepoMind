package qa

import (
	"fmt"
	"strings"

	"github.com/repomind/repomind/internal/domain/pullrequest"
)

// systemPrompt instructs the model on its role and output contract.
// The wire-level JSON Schema is enforced by the provider (strict
// mode); this prompt provides the semantic guidance the schema can't
// express.
const systemPrompt = `You are a QA test-recommendation engine for an engineering pull-request review pipeline.

Given a pull request summary, return a strict JSON object that matches the schema you have been given. Your job is to:

- Identify the impacted modules touched by this change (use the file paths as evidence).
- Identify regression areas — features or flows that could break as a side effect.
- Propose test scenarios (at least one) the QA team should run, each with:
  - a short imperative Title (<= 80 chars),
  - a one-paragraph Description of what to test and what to verify,
  - a Priority value: 1=P3 (nice to have), 2=P2 (standard), 3=P1 (must run before merge), 4=P0 (blocker, sign-off required),
  - a Module name (best-effort; "" if unclear).
- Write a Summary (1-3 sentences) that a QA lead can read in five seconds.

Be specific: cite file paths, ticket keys, or features when proposing scenarios. Do not invent functionality the diff does not show. Do not include any prose outside the JSON object.`

// Caps keep the prompt bounded so a pathological PR (10k files) can't
// blow our token budget. The values mirror the risk analyzer; promote
// to Config if a real PR exceeds them.
const (
	maxDescriptionRunes = 1500
	maxFilesListed      = 50
	maxCommitsListed    = 10
)

// buildUserPrompt renders a deterministic, structured PR summary for
// the model. Field order is fixed and lists are truncated in their
// natural order so prompt diffs across two analyzer runs of the same
// PR are minimal and reviewable.
//
// The body is intentionally the same shape as the risk analyzer's
// user prompt — a single "PR fact sheet" is enough for both classifier
// roles, and keeping them in lockstep makes prompt-diff reviews
// easier. If the two analyzers ever need different fact sheets, this
// helper can fork.
func buildUserPrompt(pr *pullrequest.PullRequest) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Pull request #%d\n", pr.Number())
	fmt.Fprintf(&b, "Title: %s\n", pr.Title())
	fmt.Fprintf(&b, "Author: %s\n", pr.Author().Login)
	fmt.Fprintf(&b, "Source -> Target: %s -> %s\n", pr.SourceBranch(), pr.TargetBranch())
	fmt.Fprintf(&b, "State: %s\n", pr.State())

	if labels := pr.Labels(); len(labels) > 0 {
		fmt.Fprintf(&b, "Labels: %s\n", strings.Join(labels, ", "))
	}

	if tickets := pr.Tickets(); len(tickets) > 0 {
		keys := make([]string, 0, len(tickets))
		for _, t := range tickets {
			keys = append(keys, t.String())
		}
		fmt.Fprintf(&b, "Tickets: %s\n", strings.Join(keys, ", "))
	}

	fmt.Fprintf(&b, "Lines changed: %d\n", pr.LinesChanged())

	if desc := truncateRunes(pr.Description(), maxDescriptionRunes); desc != "" {
		fmt.Fprintf(&b, "\nDescription:\n%s\n", desc)
	}

	files := pr.Files()
	if len(files) > 0 {
		shown := minInt(len(files), maxFilesListed)
		fmt.Fprintf(&b, "\nFiles (%d total, showing %d):\n", len(files), shown)
		for i, f := range files {
			if i >= maxFilesListed {
				break
			}
			fmt.Fprintf(&b, "- %s [%s] +%d -%d\n", f.Path, f.Status, f.Additions, f.Deletions)
		}
	}

	commits := pr.Commits()
	if len(commits) > 0 {
		shown := minInt(len(commits), maxCommitsListed)
		fmt.Fprintf(&b, "\nCommits (%d total, showing %d):\n", len(commits), shown)
		for i, c := range commits {
			if i >= maxCommitsListed {
				break
			}
			fmt.Fprintf(&b, "- %s %s\n", shortSHA(c.SHA.String()), firstLine(c.Message))
		}
	}

	reviewers := pr.Reviewers()
	if len(reviewers) > 0 {
		states := map[pullrequest.ReviewState]int{}
		for _, r := range reviewers {
			states[r.State]++
		}
		fmt.Fprintf(&b, "\nReviewers: %d total", len(reviewers))
		// Stable display order so prompt diffs are reproducible.
		for _, s := range []pullrequest.ReviewState{
			pullrequest.ReviewApproved,
			pullrequest.ReviewChangesRequested,
			pullrequest.ReviewCommented,
			pullrequest.ReviewRequested,
			pullrequest.ReviewDismissed,
		} {
			if n := states[s]; n > 0 {
				fmt.Fprintf(&b, ", %s=%d", s, n)
			}
		}
		b.WriteByte('\n')
	}

	return b.String()
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
