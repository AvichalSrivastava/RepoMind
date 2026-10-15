package releasenotes_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/repomind/repomind/internal/application/analysis/releasenotes"
	"github.com/repomind/repomind/internal/application/llm"
	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/repomind/repomind/internal/domain/sprint"
)

// ---------- fakes & helpers ----------

// fakeProvider is a stub llm.Provider used to exercise the analyzer
// without hitting a real LLM.
type fakeProvider struct {
	resp *llm.Response
	err  error

	lastReq llm.Request
}

func (f *fakeProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

var (
	fixedTime   = time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	sprintStart = time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	sprintEnd   = time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
)

func newSchema() *llm.StructuredOutputSpec {
	return &llm.StructuredOutputSpec{
		Name:   "release_notes",
		Schema: []byte(`{"type":"object"}`),
		Strict: true,
	}
}

func newClosedSprint(t *testing.T) *sprint.Sprint {
	t.Helper()
	completed := sprintEnd
	s, err := sprint.MirrorFromJira(sprint.MirrorInput{
		JiraSprintID: 187001,
		BoardID:      187,
		Team:         "Marvelous",
		Name:         "Marvelous_Sprint_42",
		State:        sprint.StateClosed,
		Goal:         "Ship the multi-team fan-out preview",
		StartsAt:     &sprintStart,
		EndsAt:       &sprintEnd,
		CompletedAt:  &completed,
	}, shared.FixedClock{T: fixedTime})
	require.NoError(t, err)
	return s
}

// newMergedPR builds a PR with the given number and one attached file.
// Each call has a fresh ID so tests can index PRs uniquely.
func newMergedPR(t *testing.T, number int, title string, tickets ...string) *pullrequest.PullRequest {
	t.Helper()
	author, err := shared.NewAuthor("alice", "Alice", "alice@example.com")
	require.NoError(t, err)

	pr, err := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: shared.NewRepositoryID(),
		Number:       pullrequest.Number(number),
		Title:        title,
		Description:  "Adds feature.",
		Author:       author,
		SourceBranch: shared.MustBranch(fmt.Sprintf("feature/%d", number)),
		TargetBranch: shared.MustBranch("main"),
		Labels:       []string{"feature"},
	}, shared.FixedClock{T: fixedTime})
	require.NoError(t, err)

	require.NoError(t, pr.AttachFile(pullrequest.ChangedFile{
		Path:   fmt.Sprintf("internal/feature/%d.go", number),
		Status: pullrequest.FileAdded, Additions: 50, Deletions: 0,
	}))

	for _, k := range tickets {
		tk, err := shared.NewTicketKey(k)
		require.NoError(t, err)
		pr.AssociateTicket(tk)
	}
	return pr
}

// happyContent returns a well-formed release-notes JSON body
// referencing prs[0] and prs[1] by ID.
func happyContent(t *testing.T, prs []*pullrequest.PullRequest) string {
	t.Helper()
	require.GreaterOrEqual(t, len(prs), 2)
	body := map[string]any{
		"Summary":  "Sprint 42 shipped the multi-team fan-out preview and a critical bug fix.",
		"Markdown": "### Features\n- Add multi-team fan-out preview\n\n### Bug fixes\n- Fix tenant isolation regression",
		"Entries": []map[string]any{
			{
				"Category":    "feature",
				"Title":       "Add multi-team fan-out preview",
				"Description": "Lets sibling teams render their own release notes.",
				"PR":          prs[0].ID().String(),
				"Tickets":     []string{"SIS-100"},
			},
			{
				"Category":    "bugfix",
				"Title":       "Fix tenant isolation regression",
				"Description": "",
				"PR":          prs[1].ID().String(),
				"Tickets":     []string{"SIS-101", "SIS-102"},
			},
		},
	}
	b, err := json.Marshal(body)
	require.NoError(t, err)
	return string(b)
}

// ---------- constructor ----------

func TestNew_RequiresProvider(t *testing.T) {
	_, err := releasenotes.New(releasenotes.Config{Schema: newSchema()})
	require.Error(t, err)
}

func TestNew_RequiresSchema(t *testing.T) {
	_, err := releasenotes.New(releasenotes.Config{Provider: &fakeProvider{}})
	require.Error(t, err)
}

func TestNew_RequiresSchemaBytes(t *testing.T) {
	spec := &llm.StructuredOutputSpec{Name: "release_notes"}
	_, err := releasenotes.New(releasenotes.Config{Provider: &fakeProvider{}, Schema: spec})
	require.Error(t, err)
}

func TestNew_RequiresSchemaName(t *testing.T) {
	spec := &llm.StructuredOutputSpec{Schema: []byte(`{"type":"object"}`)}
	_, err := releasenotes.New(releasenotes.Config{Provider: &fakeProvider{}, Schema: spec})
	require.Error(t, err)
}

func TestNew_DefaultsClock(t *testing.T) {
	a, err := releasenotes.New(releasenotes.Config{Provider: &fakeProvider{}, Schema: newSchema()})
	require.NoError(t, err)
	require.NotNil(t, a)
}

// ---------- happy path ----------

func TestAnalyze_HappyPath(t *testing.T) {
	s := newClosedSprint(t)
	prs := []*pullrequest.PullRequest{
		newMergedPR(t, 101, "feature: fan-out preview", "SIS-100"),
		newMergedPR(t, 102, "fix: tenant isolation", "SIS-101", "SIS-102"),
	}
	provider := &fakeProvider{
		resp: &llm.Response{
			Content: happyContent(t, prs),
			Model:   "gpt-4o-2024-07-18",
			Usage:   llm.Usage{PromptTokens: 800, CompletionTokens: 400, TotalTokens: 1200},
		},
	}
	a, err := releasenotes.New(releasenotes.Config{
		Provider: provider,
		Schema:   newSchema(),
		Clock:    shared.FixedClock{T: fixedTime},
	})
	require.NoError(t, err)

	result, err := a.Analyze(context.Background(), releasenotes.Input{Sprint: s, PullRequests: prs})
	require.NoError(t, err)

	require.Equal(t, analysis.KindReleaseNotes, result.Kind())
	require.NotNil(t, result.Subject().SprintID)
	require.Equal(t, s.ID(), *result.Subject().SprintID)
	require.Equal(t, releasenotes.AnalyzerName, result.Analyzer().Name)
	require.Equal(t, releasenotes.AnalyzerVersion, result.Analyzer().Version)
	require.Equal(t, releasenotes.PromptVersion, result.Analyzer().PromptVersion)
	require.Equal(t, "gpt-4o-2024-07-18", result.Model())
	require.Equal(t, 1200, result.TokensUsed())
	require.Equal(t, fixedTime, result.GeneratedAt())

	rn, ok := result.Payload().(analysis.ReleaseNotes)
	require.True(t, ok)
	require.Equal(t,
		"Sprint 42 shipped the multi-team fan-out preview and a critical bug fix.",
		rn.Summary)
	require.Contains(t, rn.Markdown, "### Features")
	require.Len(t, rn.Entries, 2)

	require.Equal(t, analysis.CategoryFeature, rn.Entries[0].Category)
	require.Equal(t, "Add multi-team fan-out preview", rn.Entries[0].Title)
	require.Equal(t, prs[0].ID(), rn.Entries[0].PR)
	require.Len(t, rn.Entries[0].Tickets, 1)
	require.Equal(t, "SIS-100", rn.Entries[0].Tickets[0].String())

	require.Equal(t, analysis.CategoryBugfix, rn.Entries[1].Category)
	require.Equal(t, prs[1].ID(), rn.Entries[1].PR)
	require.Len(t, rn.Entries[1].Tickets, 2)
}

func TestAnalyze_SendsCorrectRequest(t *testing.T) {
	s := newClosedSprint(t)
	prs := []*pullrequest.PullRequest{
		newMergedPR(t, 101, "feature: fan-out preview", "SIS-100"),
		newMergedPR(t, 102, "fix: tenant isolation", "SIS-101"),
	}
	provider := &fakeProvider{resp: &llm.Response{Content: happyContent(t, prs)}}
	schema := newSchema()
	a, err := releasenotes.New(releasenotes.Config{
		Provider:        provider,
		Schema:          schema,
		Clock:           shared.FixedClock{T: fixedTime},
		MaxOutputTokens: 4096,
		Temperature:     0.3,
	})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{Sprint: s, PullRequests: prs})
	require.NoError(t, err)

	require.Equal(t, llm.RoleReleaseNotes, provider.lastReq.ModelRole)
	require.Same(t, schema, provider.lastReq.StructuredOutput)
	require.Equal(t, 4096, provider.lastReq.MaxOutputTokens)
	require.InDelta(t, 0.3, provider.lastReq.Temperature, 0.0001)

	require.Len(t, provider.lastReq.Messages, 2)
	require.Equal(t, llm.RoleSystem, provider.lastReq.Messages[0].Role)
	require.Equal(t, llm.RoleUser, provider.lastReq.Messages[1].Role)

	user := provider.lastReq.Messages[1].Content
	require.Contains(t, user, "Sprint: Marvelous_Sprint_42")
	require.Contains(t, user, "Team: Marvelous")
	require.Contains(t, user, "Goal: Ship the multi-team fan-out preview")
	require.Contains(t, user, "Dates: 2026-06-15 -> 2026-06-28")
	require.Contains(t, user, "Pull requests (2 total, showing 2):")
	require.Contains(t, user, "PR #101")
	require.Contains(t, user, "PR #102")
	require.Contains(t, user, prs[0].ID().String())
	require.Contains(t, user, prs[1].ID().String())
	require.Contains(t, user, "SIS-100")
}

// ---------- input validation ----------

func TestAnalyze_NilSprint(t *testing.T) {
	a, err := releasenotes.New(releasenotes.Config{Provider: &fakeProvider{}, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       nil,
		PullRequests: []*pullrequest.PullRequest{newMergedPR(t, 1, "x")},
	})
	require.Error(t, err)
}

func TestAnalyze_EmptyPRs(t *testing.T) {
	a, err := releasenotes.New(releasenotes.Config{Provider: &fakeProvider{}, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: nil,
	})
	require.Error(t, err)
}

func TestAnalyze_NilPRInSlice(t *testing.T) {
	a, err := releasenotes.New(releasenotes.Config{Provider: &fakeProvider{}, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: []*pullrequest.PullRequest{newMergedPR(t, 1, "x"), nil},
	})
	require.Error(t, err)
}

// ---------- error propagation ----------

func TestAnalyze_PropagatesLLMError(t *testing.T) {
	provider := &fakeProvider{err: llm.ErrUnauthorized}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: []*pullrequest.PullRequest{newMergedPR(t, 1, "x")},
	})
	require.ErrorIs(t, err, llm.ErrUnauthorized)
}

func TestAnalyze_MalformedJSON(t *testing.T) {
	provider := &fakeProvider{resp: &llm.Response{Content: "not json"}}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: []*pullrequest.PullRequest{newMergedPR(t, 1, "x")},
	})
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_EmptySummaryFails(t *testing.T) {
	pr := newMergedPR(t, 1, "x")
	provider := &fakeProvider{resp: &llm.Response{
		Content: fmt.Sprintf(`{"Summary":"","Markdown":"# rn","Entries":[{"Category":"feature","Title":"t","Description":"","PR":"%s","Tickets":[]}]}`, pr.ID().String()),
	}}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: []*pullrequest.PullRequest{pr},
	})
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_EmptyMarkdownFails(t *testing.T) {
	pr := newMergedPR(t, 1, "x")
	provider := &fakeProvider{resp: &llm.Response{
		Content: fmt.Sprintf(`{"Summary":"s","Markdown":"","Entries":[{"Category":"feature","Title":"t","Description":"","PR":"%s","Tickets":[]}]}`, pr.ID().String()),
	}}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: []*pullrequest.PullRequest{pr},
	})
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_NoEntriesFails(t *testing.T) {
	provider := &fakeProvider{resp: &llm.Response{
		Content: `{"Summary":"s","Markdown":"# rn","Entries":[]}`,
	}}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: []*pullrequest.PullRequest{newMergedPR(t, 1, "x")},
	})
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_InvalidCategoryFails(t *testing.T) {
	pr := newMergedPR(t, 1, "x")
	provider := &fakeProvider{resp: &llm.Response{
		Content: fmt.Sprintf(`{"Summary":"s","Markdown":"# rn","Entries":[{"Category":"banana","Title":"t","Description":"","PR":"%s","Tickets":[]}]}`, pr.ID().String()),
	}}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: []*pullrequest.PullRequest{pr},
	})
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_EmptyEntryTitleFails(t *testing.T) {
	pr := newMergedPR(t, 1, "x")
	provider := &fakeProvider{resp: &llm.Response{
		Content: fmt.Sprintf(`{"Summary":"s","Markdown":"# rn","Entries":[{"Category":"feature","Title":"","Description":"","PR":"%s","Tickets":[]}]}`, pr.ID().String()),
	}}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: []*pullrequest.PullRequest{pr},
	})
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_InvalidPRIDFails(t *testing.T) {
	// Not a UUID — decode-side parse should reject it.
	provider := &fakeProvider{resp: &llm.Response{
		Content: `{"Summary":"s","Markdown":"# rn","Entries":[{"Category":"feature","Title":"t","Description":"","PR":"not-a-uuid","Tickets":[]}]}`,
	}}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: []*pullrequest.PullRequest{newMergedPR(t, 1, "x")},
	})
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_InvalidTicketKeyFails(t *testing.T) {
	pr := newMergedPR(t, 1, "x")
	provider := &fakeProvider{resp: &llm.Response{
		Content: fmt.Sprintf(`{"Summary":"s","Markdown":"# rn","Entries":[{"Category":"feature","Title":"t","Description":"","PR":"%s","Tickets":["lowercase-bad"]}]}`, pr.ID().String()),
	}}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: []*pullrequest.PullRequest{pr},
	})
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

// ---------- corner cases ----------

func TestAnalyze_NilProviderResponse(t *testing.T) {
	provider := &fakeProvider{resp: nil, err: nil}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: []*pullrequest.PullRequest{newMergedPR(t, 1, "x")},
	})
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_StripsCodeFence(t *testing.T) {
	s := newClosedSprint(t)
	prs := []*pullrequest.PullRequest{
		newMergedPR(t, 1, "f1", "SIS-100"),
		newMergedPR(t, 2, "f2", "SIS-101"),
	}
	body := happyContent(t, prs)
	tests := []struct {
		name    string
		content string
	}{
		{"fenced with json tag", "```json\n" + body + "\n```"},
		{"fenced without tag", "```\n" + body + "\n```"},
		{"fenced with surrounding whitespace", "\n\n```json\n" + body + "\n```\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider := &fakeProvider{resp: &llm.Response{Content: tc.content}}
			a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
			require.NoError(t, err)

			res, err := a.Analyze(context.Background(), releasenotes.Input{Sprint: s, PullRequests: prs})
			require.NoError(t, err)
			rn := res.Payload().(analysis.ReleaseNotes)
			require.Len(t, rn.Entries, 2)
		})
	}
}

func TestAnalyze_PropagatesContextCancellation(t *testing.T) {
	provider := &fakeProvider{err: context.Canceled}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: []*pullrequest.PullRequest{newMergedPR(t, 1, "x")},
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestAnalyze_PropagatesContentFiltered(t *testing.T) {
	provider := &fakeProvider{err: llm.ErrContentFiltered}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{
		Sprint:       newClosedSprint(t),
		PullRequests: []*pullrequest.PullRequest{newMergedPR(t, 1, "x")},
	})
	require.ErrorIs(t, err, llm.ErrContentFiltered)
}

// Prompt must cap at 100 PRs even when more are supplied, so a
// pathological sprint can't blow our token budget. We assert by
// checking the captured user prompt: PR #100 appears, PR #101 does not.
func TestAnalyze_TruncatesPRsBeyondCap(t *testing.T) {
	const total = 150
	s := newClosedSprint(t)
	prs := make([]*pullrequest.PullRequest, 0, total)
	for i := 1; i <= total; i++ {
		prs = append(prs, newMergedPR(t, i, fmt.Sprintf("change %d", i)))
	}
	// Build a minimal happy response that references the first two PRs.
	provider := &fakeProvider{resp: &llm.Response{Content: happyContent(t, prs)}}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), releasenotes.Input{Sprint: s, PullRequests: prs})
	require.NoError(t, err)

	user := provider.lastReq.Messages[1].Content
	require.Contains(t, user, "Pull requests (150 total, showing 100):")
	require.Contains(t, user, "PR #100")
	require.False(t, strings.Contains(user, "PR #101"),
		"PR #101 must be truncated when the cap is 100")
}

// ---------- prompt-rendering branch coverage ----------

// openPR builds a minimal PR with an override-able description and no
// file/label noise, so prompt-rendering branches can be exercised
// without the defaults baked into newMergedPR.
func openPR(t *testing.T, number int, title, description string, tickets ...string) *pullrequest.PullRequest {
	t.Helper()
	author, err := shared.NewAuthor("alice", "Alice", "alice@example.com")
	require.NoError(t, err)
	pr, err := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: shared.NewRepositoryID(),
		Number:       pullrequest.Number(number),
		Title:        title,
		Description:  description,
		Author:       author,
		SourceBranch: shared.MustBranch(fmt.Sprintf("feature/%d", number)),
		TargetBranch: shared.MustBranch("main"),
	}, shared.FixedClock{T: fixedTime})
	require.NoError(t, err)
	for _, k := range tickets {
		tk, err := shared.NewTicketKey(k)
		require.NoError(t, err)
		pr.AssociateTicket(tk)
	}
	return pr
}

// newActiveSprint builds an active sprint with caller-controlled goal
// and dates so we can exercise the optional-field branches in the
// prompt builder.
func newActiveSprint(t *testing.T, goal string, starts, ends *time.Time) *sprint.Sprint {
	t.Helper()
	s, err := sprint.MirrorFromJira(sprint.MirrorInput{
		JiraSprintID: 187002,
		BoardID:      187,
		Team:         "Marvelous",
		Name:         "Marvelous_Sprint_43",
		State:        sprint.StateActive,
		Goal:         goal,
		StartsAt:     starts,
		EndsAt:       ends,
	}, shared.FixedClock{T: fixedTime})
	require.NoError(t, err)
	return s
}

// We inspect provider.lastReq for prompt assertions; the provider's
// return path doesn't matter, so a transport error is the cleanest
// signal: Analyze still builds and sends the prompt before failing.
func captureUserPrompt(t *testing.T, s *sprint.Sprint, prs []*pullrequest.PullRequest) string {
	t.Helper()
	provider := &fakeProvider{err: llm.ErrUnauthorized}
	a, err := releasenotes.New(releasenotes.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)
	_, _ = a.Analyze(context.Background(), releasenotes.Input{Sprint: s, PullRequests: prs})
	require.NotEmpty(t, provider.lastReq.Messages, "Analyze must have called the provider")
	return provider.lastReq.Messages[1].Content
}

func TestAnalyze_PromptOmitsGoalWhenBlank(t *testing.T) {
	s := newActiveSprint(t, "   ", &sprintStart, &sprintEnd) // whitespace-only goal
	user := captureUserPrompt(t, s, []*pullrequest.PullRequest{newMergedPR(t, 1, "x")})

	require.NotContains(t, user, "Goal:", "Goal: line must be omitted when sprint goal is blank")
}

func TestAnalyze_PromptRendersUnknownForNilSprintDates(t *testing.T) {
	s := newActiveSprint(t, "ship preview", nil, nil)
	user := captureUserPrompt(t, s, []*pullrequest.PullRequest{newMergedPR(t, 1, "x")})

	require.Contains(t, user, "Dates: (unknown) -> (unknown)")
}

func TestAnalyze_PromptTruncatesLongPRDescription(t *testing.T) {
	// 350 rune description; cap is 300 → expect trailing "…" and
	// the 301st rune (a unique marker char) NOT to appear.
	long := strings.Repeat("a", 300) + strings.Repeat("Z", 50)
	pr := openPR(t, 7, "feature: x", long, "SIS-1")
	user := captureUserPrompt(t, newClosedSprint(t), []*pullrequest.PullRequest{pr})

	require.Contains(t, user, "Description: "+strings.Repeat("a", 300)+"…")
	require.NotContains(t, user, "Z", "runes beyond the cap must not leak into the prompt")
}

func TestAnalyze_PromptCollapsesMultilinePRDescription(t *testing.T) {
	pr := openPR(t, 8, "feature: y", "line one\nline two\r\nline three")
	user := captureUserPrompt(t, newClosedSprint(t), []*pullrequest.PullRequest{pr})

	require.Contains(t, user, "Description: line one line two line three")
}

func TestAnalyze_PromptTruncatesManyTicketsPerPR(t *testing.T) {
	// 15 tickets; cap is 10 → only the first 10 appear, and the 11th
	// (SIS-11) must NOT appear.
	keys := make([]string, 0, 15)
	for i := 1; i <= 15; i++ {
		keys = append(keys, fmt.Sprintf("SIS-%d", i))
	}
	pr := openPR(t, 9, "feature: z", "desc", keys...)
	user := captureUserPrompt(t, newClosedSprint(t), []*pullrequest.PullRequest{pr})

	require.Contains(t, user, "SIS-1,")
	require.Contains(t, user, "SIS-10")
	require.NotContains(t, user, "SIS-11", "ticket #11 must be truncated when the per-PR cap is 10")
}

func TestAnalyze_PromptHandlesPRWithMinimalMetadata(t *testing.T) {
	// No description, no labels, no tickets, no files attached.
	pr := openPR(t, 10, "chore: bump deps", "")
	user := captureUserPrompt(t, newClosedSprint(t), []*pullrequest.PullRequest{pr})

	require.Contains(t, user, "PR #10")
	require.Contains(t, user, "Files: 0 changed")
	require.Contains(t, user, "Lines changed: 0")
	require.NotContains(t, user, "Labels:")
	require.NotContains(t, user, "Tickets:")
	require.NotContains(t, user, "Description:")
}
