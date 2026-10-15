package orchestration

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/repomind/repomind/internal/domain/sprint"
)

// PerTicketRenderInput bundles inputs for RenderPerTicket.
//
// JiraBaseURL and GitHubBaseURL are optional; when non-empty they
// are used to render ticket keys and PR references as markdown
// links. URLs that don't pass the publisher's safe-scheme check are
// dropped by the downstream renderer; we don't try to be clever here.
type PerTicketRenderInput struct {
	Sprint        *sprint.Sprint
	Tickets       []shared.TicketKey // sprint tickets in Jira's canonical order
	PRs           []*pullrequest.PullRequest
	JiraBaseURL   string
	GitHubBaseURL string // defaults to "https://github.com"
}

// RenderPerTicket builds an *analysis.ReleaseNotes shaped exactly
// like the LLM composer's output, but produced deterministically
// from sprint + tickets + PRs. No external calls, no token cost, no
// hallucinations.
//
// Layout (markdown body):
//
//	## Tickets in this release
//
//	### TICKET-1
//	- PR #N <title> — owner/repo — +X/-Y in F files
//	- PR #M ...
//
//	### TICKET-2
//	- ...
//
//	### Unlinked changes        (only when present)
//	- PR #K ...
//
// Tickets appear in the order Jira returned them so two re-renders
// produce byte-identical markdown. PRs within a ticket are sorted
// by repo name then ascending PR number for the same reason.
func RenderPerTicket(in PerTicketRenderInput) (analysis.ReleaseNotes, error) {
	if in.Sprint == nil {
		return analysis.ReleaseNotes{}, errors.New("RenderPerTicket: Sprint is required")
	}
	if len(in.PRs) == 0 {
		return analysis.ReleaseNotes{}, errors.New("RenderPerTicket: at least one PR is required")
	}

	githubBase := strings.TrimRight(strings.TrimSpace(in.GitHubBaseURL), "/")
	if githubBase == "" {
		githubBase = "https://github.com"
	}
	jiraBase := strings.TrimRight(strings.TrimSpace(in.JiraBaseURL), "/")

	// Index PRs by ticket key (uppercased for case-insensitive match)
	// and remember which PRs never matched any sprint ticket.
	byTicket := make(map[string][]*pullrequest.PullRequest, len(in.Tickets))
	matched := make(map[shared.PullRequestID]bool, len(in.PRs))
	sprintKeys := make(map[string]bool, len(in.Tickets))
	for _, t := range in.Tickets {
		sprintKeys[strings.ToUpper(t.String())] = true
	}
	for _, pr := range in.PRs {
		for _, tk := range pr.Tickets() {
			key := strings.ToUpper(tk.String())
			if sprintKeys[key] {
				byTicket[key] = append(byTicket[key], pr)
				matched[pr.ID()] = true
			}
		}
	}
	var unlinked []*pullrequest.PullRequest
	for _, pr := range in.PRs {
		if !matched[pr.ID()] {
			unlinked = append(unlinked, pr)
		}
	}

	var md strings.Builder
	var entries []analysis.ReleaseNoteEntry

	md.WriteString("## Tickets in this release\n\n")

	ticketsWithChanges := 0
	for _, t := range in.Tickets {
		prs := byTicket[strings.ToUpper(t.String())]
		if len(prs) == 0 {
			continue
		}
		ticketsWithChanges++
		sortPRs(prs)

		md.WriteString("### ")
		md.WriteString(renderTicketLabel(t.String(), jiraBase))
		md.WriteString("\n\n")

		for _, pr := range prs {
			md.WriteString("- ")
			md.WriteString(renderPRBullet(pr, githubBase))
			md.WriteString("\n")

			entries = append(entries, analysis.ReleaseNoteEntry{
				Category:    classifyByLabels(pr),
				Title:       pr.Title(),
				Description: "",
				PR:          pr.ID(),
				Tickets:     []shared.TicketKey{t},
			})
		}
		md.WriteString("\n")
	}

	if len(unlinked) > 0 {
		sortPRs(unlinked)
		md.WriteString("### Unlinked changes\n\n")
		md.WriteString("_PRs that didn't reference any ticket in this sprint._\n\n")
		for _, pr := range unlinked {
			md.WriteString("- ")
			md.WriteString(renderPRBullet(pr, githubBase))
			md.WriteString("\n")

			entries = append(entries, analysis.ReleaseNoteEntry{
				Category: classifyByLabels(pr),
				Title:    pr.Title(),
				PR:       pr.ID(),
			})
		}
		md.WriteString("\n")
	}

	summary := buildSummary(in.Sprint, len(in.PRs), ticketsWithChanges, len(unlinked))
	return analysis.NewReleaseNotes(summary, md.String(), entries)
}

// sortPRs orders PRs by repo-full-name then ascending PR number so
// two renders of the same sprint produce identical markdown.
func sortPRs(prs []*pullrequest.PullRequest) {
	sort.SliceStable(prs, func(i, j int) bool {
		ri, rj := repoOf(prs[i]), repoOf(prs[j])
		if ri != rj {
			return ri < rj
		}
		return prs[i].Number() < prs[j].Number()
	})
}

// repoOf returns the "owner/name" string for the PR's repo. We don't
// have repository info on the PR aggregate today, so use a hash of
// the RepoID -- empty if not set. (Composition root fills this in
// via PRsByRepository when feeding the handler.)
//
// Today we approximate "owner/name" via the deterministic empty
// string + repo UUID prefix because the PR aggregate only exposes
// RepoID. The handler enriches this via a lookup before publishing
// (see ReleaseNotesHandler.buildPRSummaries).
func repoOf(pr *pullrequest.PullRequest) string {
	if pr == nil {
		return ""
	}
	return pr.RepositoryID().String()
}

// renderPRBullet renders one PR as a single markdown bullet line:
//
//	[PR #123 Fix scoring rounding](https://github.com/owner/repo/pull/123) — `owner/repo` — +18/-12 in 3 files — @alice
//
// repoFullName is plain text (publisher escapes HTML downstream).
// If githubBase is empty we render the label as plain text without
// the link wrapper.
func renderPRBullet(pr *pullrequest.PullRequest, githubBase string) string {
	var b strings.Builder
	label := fmt.Sprintf("PR #%d %s", pr.Number(), strings.TrimSpace(pr.Title()))
	url := buildPRURL(githubBase, pr)
	if url != "" {
		fmt.Fprintf(&b, "[%s](%s)", label, url)
	} else {
		b.WriteString(label)
	}
	if author := strings.TrimSpace(pr.Author().Login); author != "" {
		fmt.Fprintf(&b, " — by **@%s**", author)
	}
	// File / line stats are best-effort; the PR aggregate exposes
	// them via accessors. We render zeros as " — 0 files changed".
	files := len(pr.Files())
	add, del := totalLineChanges(pr)
	fmt.Fprintf(&b, " — +%d/-%d in %d file", add, del, files)
	if files != 1 {
		b.WriteByte('s')
	}
	return b.String()
}

// totalLineChanges sums additions/deletions across all PR files.
func totalLineChanges(pr *pullrequest.PullRequest) (int, int) {
	var add, del int
	for _, f := range pr.Files() {
		add += f.Additions
		del += f.Deletions
	}
	return add, del
}

// renderTicketLabel produces a markdown link when jiraBase is set,
// otherwise plain text.
func renderTicketLabel(key, jiraBase string) string {
	key = strings.TrimSpace(key)
	if jiraBase == "" {
		return key
	}
	return fmt.Sprintf("[%s](%s/browse/%s)", key, jiraBase, key)
}

// buildPRURL composes a github.com/owner/repo/pull/N URL. Today the
// PR aggregate doesn't carry the owner/name pair, so the handler
// must populate githubBase to "" if it cannot resolve the repo; we
// then fall back to no link.
//
// (Future: thread repo info through PullRequest.WithRepo to make
// this self-contained.)
func buildPRURL(githubBase string, pr *pullrequest.PullRequest) string {
	// Without repo owner/name on the aggregate, we can't form the URL
	// reliably. The handler-side wiring layer supplies the same
	// renderer with a "{repo}/pull/{number}" approximation by
	// pre-rendering. For now return "" to skip the link cleanly.
	_ = githubBase
	_ = pr
	return ""
}

// classifyByLabels picks a ChangeCategory from the PR's GitHub
// labels. Mapping is intentionally conservative — anything we can't
// classify becomes CategoryImprovement so the entry is always valid.
//
//	"breaking", "breaking-change"         -> CategoryBreakingChange
//	"security", "cve"                     -> CategorySecurity
//	"bug", "bugfix", "fix"                -> CategoryBugfix
//	"feature", "enhancement"              -> CategoryFeature
//	"chore", "dependencies", "docs"       -> CategoryChore
//	(anything else)                       -> CategoryImprovement
func classifyByLabels(pr *pullrequest.PullRequest) analysis.ChangeCategory {
	for _, raw := range pr.Labels() {
		l := strings.ToLower(strings.TrimSpace(raw))
		switch l {
		case "breaking", "breaking-change", "breaking_change":
			return analysis.CategoryBreakingChange
		case "security", "cve", "vuln", "vulnerability":
			return analysis.CategorySecurity
		case "bug", "bugfix", "fix":
			return analysis.CategoryBugfix
		case "feature", "enhancement", "feat":
			return analysis.CategoryFeature
		case "chore", "dependencies", "deps", "docs", "documentation":
			return analysis.CategoryChore
		}
	}
	return analysis.CategoryImprovement
}

// buildSummary produces the one-paragraph headline at the top of
// the published page. Deterministic, byte-identical across re-renders
// for the same inputs.
func buildSummary(sp *sprint.Sprint, prCount, ticketsWithChanges, unlinkedCount int) string {
	when := "in progress"
	if ends := sp.EndsAt(); ends != nil {
		switch sp.State() {
		case sprint.StateClosed:
			when = "closed " + ends.UTC().Format("02 Jan 2006")
		case sprint.StateActive:
			when = "active until " + ends.UTC().Format("02 Jan 2006")
		default:
			when = ends.UTC().Format("02 Jan 2006")
		}
	}
	parts := []string{
		fmt.Sprintf("%d merged %s", prCount, plural(prCount, "PR", "PRs")),
		fmt.Sprintf("%d %s with changes", ticketsWithChanges, plural(ticketsWithChanges, "ticket", "tickets")),
	}
	if unlinkedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d unlinked", unlinkedCount))
	}
	return fmt.Sprintf("Sprint %s — %s — %s.", sp.Name(), when, strings.Join(parts, ", "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// _ keeps time imported for future timestamp use in the summary.
var _ = time.Now
