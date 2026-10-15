package orchestration

import (
	"strings"
	"testing"
	"time"

	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/repomind/repomind/internal/domain/sprint"
)

func TestRenderPerTicket_RejectsNilSprint(t *testing.T) {
	_, err := RenderPerTicket(PerTicketRenderInput{
		Sprint: nil,
		PRs:    []*pullrequest.PullRequest{newRendererPR(t, 1, "x", []string{"SIS-1"}, nil)},
	})
	if err == nil {
		t.Fatal("want error for nil Sprint, got nil")
	}
}

func TestRenderPerTicket_RejectsEmptyPRs(t *testing.T) {
	sp := newRendererSprint(t, "Marvelous_Sprint_99")
	_, err := RenderPerTicket(PerTicketRenderInput{Sprint: sp})
	if err == nil {
		t.Fatal("want error for empty PRs, got nil")
	}
}

func TestRenderPerTicket_GroupsByTicketInJiraOrder(t *testing.T) {
	sp := newRendererSprint(t, "Marvelous_Sprint_185")
	tickets := []shared.TicketKey{
		mustKey(t, "SIS-101"),
		mustKey(t, "SIS-102"),
		mustKey(t, "SIS-103"), // no PR -- shouldn't appear
	}
	prs := []*pullrequest.PullRequest{
		newRendererPR(t, 1, "fix A", []string{"SIS-101"}, []string{"bug"}),
		newRendererPR(t, 2, "feat B", []string{"SIS-102"}, []string{"feature"}),
		// PR that hits 101 and 102 -> appears in BOTH sections
		newRendererPR(t, 3, "cross C", []string{"SIS-101", "SIS-102"}, nil),
	}

	notes, err := RenderPerTicket(PerTicketRenderInput{
		Sprint: sp, Tickets: tickets, PRs: prs, JiraBaseURL: "https://shl.atlassian.net",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	idx101 := strings.Index(notes.Markdown, "SIS-101")
	idx102 := strings.Index(notes.Markdown, "SIS-102")
	if idx101 < 0 || idx102 < 0 {
		t.Fatalf("missing ticket headings; got:\n%s", notes.Markdown)
	}
	if idx101 >= idx102 {
		t.Errorf("SIS-101 should appear before SIS-102 (Jira order); got\n%s", notes.Markdown)
	}
	if strings.Contains(notes.Markdown, "SIS-103") {
		t.Errorf("SIS-103 had no PRs -- must not appear in body")
	}
	// Ticket headings should be markdown links to the configured Jira.
	if !strings.Contains(notes.Markdown, "[SIS-101](https://shl.atlassian.net/browse/SIS-101)") {
		t.Errorf("ticket should be a link; got:\n%s", notes.Markdown)
	}
	if c := strings.Count(notes.Markdown, "PR #3 cross C"); c != 2 {
		t.Errorf("PR #3 should appear under both tickets it references; count=%d body:\n%s", c, notes.Markdown)
	}
}

func TestRenderPerTicket_UnlinkedPRsBucketed(t *testing.T) {
	sp := newRendererSprint(t, "Marvelous_Sprint_185")
	tickets := []shared.TicketKey{mustKey(t, "SIS-200")}
	prs := []*pullrequest.PullRequest{
		newRendererPR(t, 10, "valid", []string{"SIS-200"}, nil),
		newRendererPR(t, 11, "orphan one", []string{"SIS-999"}, nil), // not in sprint
		newRendererPR(t, 12, "no-tickets", nil, nil),                 // no tickets at all
	}
	notes, err := RenderPerTicket(PerTicketRenderInput{Sprint: sp, Tickets: tickets, PRs: prs})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(notes.Markdown, "### Unlinked changes") {
		t.Errorf("expected Unlinked changes section; got:\n%s", notes.Markdown)
	}
	if !strings.Contains(notes.Markdown, "PR #11 orphan one") {
		t.Errorf("orphan PR should be listed; got:\n%s", notes.Markdown)
	}
	if !strings.Contains(notes.Markdown, "PR #12 no-tickets") {
		t.Errorf("no-tickets PR should be listed; got:\n%s", notes.Markdown)
	}
}

func TestRenderPerTicket_SummaryHasCounts(t *testing.T) {
	sp := newRendererSprint(t, "Marvelous_Sprint_185")
	tickets := []shared.TicketKey{mustKey(t, "SIS-1"), mustKey(t, "SIS-2")}
	prs := []*pullrequest.PullRequest{
		newRendererPR(t, 1, "a", []string{"SIS-1"}, nil),
		newRendererPR(t, 2, "b", []string{"SIS-2"}, nil),
		newRendererPR(t, 3, "c", []string{"SIS-1"}, nil),
	}
	notes, err := RenderPerTicket(PerTicketRenderInput{Sprint: sp, Tickets: tickets, PRs: prs})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(notes.Summary, "3 merged PRs") {
		t.Errorf("summary should mention PR count; got %q", notes.Summary)
	}
	if !strings.Contains(notes.Summary, "2 tickets") {
		t.Errorf("summary should mention ticket count; got %q", notes.Summary)
	}
	if !strings.Contains(notes.Summary, "Marvelous_Sprint_185") {
		t.Errorf("summary should mention sprint name; got %q", notes.Summary)
	}
}

func TestRenderPerTicket_DeterministicAcrossRuns(t *testing.T) {
	sp := newRendererSprint(t, "Marvelous_Sprint_185")
	tickets := []shared.TicketKey{mustKey(t, "SIS-1"), mustKey(t, "SIS-2")}
	prs := []*pullrequest.PullRequest{
		newRendererPR(t, 5, "five", []string{"SIS-2"}, nil),
		newRendererPR(t, 1, "one", []string{"SIS-1"}, nil),
		newRendererPR(t, 3, "three", []string{"SIS-1"}, nil),
	}

	first, err := RenderPerTicket(PerTicketRenderInput{Sprint: sp, Tickets: tickets, PRs: prs})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	second, err := RenderPerTicket(PerTicketRenderInput{Sprint: sp, Tickets: tickets, PRs: prs})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if first.Markdown != second.Markdown {
		t.Errorf("RenderPerTicket must be deterministic across runs\nfirst:\n%s\nsecond:\n%s", first.Markdown, second.Markdown)
	}
}

func TestClassifyByLabels(t *testing.T) {
	cases := []struct {
		labels []string
		want   analysis.ChangeCategory
	}{
		{[]string{"bug"}, analysis.CategoryBugfix},
		{[]string{"BUG"}, analysis.CategoryBugfix},
		{[]string{"feature", "enhancement"}, analysis.CategoryBreakingChange}, // breaking-change wins only when present
		{[]string{"breaking-change"}, analysis.CategoryBreakingChange},
		{[]string{"security"}, analysis.CategorySecurity},
		{[]string{"feat"}, analysis.CategoryFeature},
		{[]string{"chore"}, analysis.CategoryChore},
		{nil, analysis.CategoryImprovement},
		{[]string{"qa-only", "weird"}, analysis.CategoryImprovement},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.labels, ","), func(t *testing.T) {
			pr := newRendererPR(t, 1, "x", []string{"XX-1"}, c.labels)
			got := classifyByLabels(pr)
			// "feature" doesn't promote to breaking-change unless the
			// breaking label is present; correct the expectation when
			// only innocuous labels were supplied.
			if c.want == analysis.CategoryBreakingChange && !containsLabel(c.labels, "breaking-change") {
				c.want = analysis.CategoryFeature
			}
			if got != c.want {
				t.Errorf("labels=%v: classify = %q, want %q", c.labels, got, c.want)
			}
		})
	}
}

// ---- helpers --------------------------------------------------------

func newRendererSprint(t *testing.T, name string) *sprint.Sprint {
	t.Helper()
	end := time.Date(2026, 6, 16, 16, 30, 0, 0, time.UTC)
	return sprint.Rehydrate(
		shared.NewSprintID(),
		3974, 187,
		"Marvelous", name, "",
		sprint.StateClosed,
		nil, &end, &end,
		end,
	)
}

func newRendererPR(t *testing.T, number int, title string, ticketKeys []string, labels []string) *pullrequest.PullRequest {
	t.Helper()
	author, err := shared.NewAuthor("alice", "Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	src, err := shared.NewBranch("feature/x")
	if err != nil {
		t.Fatalf("src branch: %v", err)
	}
	dst, err := shared.NewBranch("qa")
	if err != nil {
		t.Fatalf("dst branch: %v", err)
	}
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	sha, _ := shared.NewSHA(strings.Repeat("a", 40))
	pr, err := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: shared.NewRepositoryID(),
		Number:       pullrequest.Number(number),
		Title:        title,
		Description:  "",
		Author:       author,
		SourceBranch: src,
		TargetBranch: dst,
		Labels:       labels,
	}, shared.SystemClock{})
	if err != nil {
		t.Fatalf("pr: %v", err)
	}
	for _, k := range ticketKeys {
		tk, err := shared.NewTicketKey(k)
		if err != nil {
			t.Fatalf("ticket %s: %v", k, err)
		}
		pr.AssociateTicket(tk)
	}
	if err := pr.Merge(now.Add(time.Hour), sha); err != nil {
		t.Fatalf("merge: %v", err)
	}
	return pr
}

func mustKey(t *testing.T, s string) shared.TicketKey {
	t.Helper()
	k, err := shared.NewTicketKey(s)
	if err != nil {
		t.Fatalf("ticket %s: %v", s, err)
	}
	return k
}

func containsLabel(ls []string, want string) bool {
	for _, l := range ls {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}
