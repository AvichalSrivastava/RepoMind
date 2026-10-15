package risk_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/repomind/repomind/internal/application/analysis/risk"
	"github.com/repomind/repomind/internal/application/llm"
	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/shared"
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

var fixedTime = time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

func newSchema() *llm.StructuredOutputSpec {
	return &llm.StructuredOutputSpec{
		Name:   "risk_report",
		Schema: []byte(`{"type":"object"}`),
		Strict: true,
	}
}

func newOpenPR(t *testing.T) *pullrequest.PullRequest {
	t.Helper()
	author, err := shared.NewAuthor("alice", "Alice", "alice@example.com")
	require.NoError(t, err)

	pr, err := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: shared.NewRepositoryID(),
		Number:       42,
		Title:        "Add feature X",
		Description:  "Adds the X module to support multi-team fan-out.",
		Author:       author,
		SourceBranch: shared.MustBranch("feature/x"),
		TargetBranch: shared.MustBranch("main"),
		Labels:       []string{"feature", "needs-review"},
	}, shared.FixedClock{T: fixedTime})
	require.NoError(t, err)

	require.NoError(t, pr.AttachFile(pullrequest.ChangedFile{
		Path: "internal/x/x.go", Status: pullrequest.FileAdded, Additions: 120, Deletions: 0,
	}))
	require.NoError(t, pr.AttachFile(pullrequest.ChangedFile{
		Path: "migrations/000003_x.up.sql", Status: pullrequest.FileAdded, Additions: 25, Deletions: 0,
	}))

	tk, err := shared.NewTicketKey("SIS-100")
	require.NoError(t, err)
	pr.AssociateTicket(tk)
	return pr
}

// ---------- constructor ----------

func TestNew_RequiresProvider(t *testing.T) {
	_, err := risk.New(risk.Config{Schema: newSchema()})
	require.Error(t, err)
}

func TestNew_RequiresSchema(t *testing.T) {
	_, err := risk.New(risk.Config{Provider: &fakeProvider{}})
	require.Error(t, err)
}

func TestNew_DefaultsClock(t *testing.T) {
	a, err := risk.New(risk.Config{Provider: &fakeProvider{}, Schema: newSchema()})
	require.NoError(t, err)
	require.NotNil(t, a)
}

// ---------- happy path ----------

func TestAnalyze_HappyPath(t *testing.T) {
	provider := &fakeProvider{
		resp: &llm.Response{
			Content: `{"Level":3,"Factors":["database_migration","large_diff"],"Explanation":"Schema migration with a sizeable diff.","Confidence":0.8}`,
			Model:   "gpt-4o-mini-2024-07-18",
			Usage:   llm.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		},
	}
	a, err := risk.New(risk.Config{
		Provider: provider,
		Schema:   newSchema(),
		Clock:    shared.FixedClock{T: fixedTime},
	})
	require.NoError(t, err)

	pr := newOpenPR(t)
	result, err := a.Analyze(context.Background(), pr)
	require.NoError(t, err)

	require.Equal(t, analysis.KindRisk, result.Kind())
	require.NotNil(t, result.Subject().PullRequestID)
	require.Equal(t, pr.ID(), *result.Subject().PullRequestID)
	require.Equal(t, risk.AnalyzerName, result.Analyzer().Name)
	require.Equal(t, risk.AnalyzerVersion, result.Analyzer().Version)
	require.Equal(t, risk.PromptVersion, result.Analyzer().PromptVersion)
	require.Equal(t, "gpt-4o-mini-2024-07-18", result.Model())
	require.Equal(t, 150, result.TokensUsed())
	require.Equal(t, fixedTime, result.GeneratedAt())

	rr, ok := result.Payload().(analysis.RiskReport)
	require.True(t, ok)
	require.Equal(t, analysis.RiskHigh, rr.Level)
	require.ElementsMatch(t,
		[]analysis.RiskFactor{analysis.FactorDatabaseMigration, analysis.FactorLargeDiff},
		rr.Factors)
	require.InDelta(t, 0.8, rr.Confidence, 0.0001)
	require.Equal(t, "Schema migration with a sizeable diff.", rr.Explanation)
}

func TestAnalyze_SendsCorrectRequest(t *testing.T) {
	provider := &fakeProvider{
		resp: &llm.Response{
			Content: `{"Level":1,"Factors":[],"Explanation":"Trivial change.","Confidence":0.9}`,
		},
	}
	schema := newSchema()
	a, err := risk.New(risk.Config{
		Provider:        provider,
		Schema:          schema,
		Clock:           shared.FixedClock{T: fixedTime},
		MaxOutputTokens: 512,
		Temperature:     0.1,
	})
	require.NoError(t, err)

	pr := newOpenPR(t)
	_, err = a.Analyze(context.Background(), pr)
	require.NoError(t, err)

	require.Equal(t, llm.RoleRisk, provider.lastReq.ModelRole)
	require.Same(t, schema, provider.lastReq.StructuredOutput)
	require.Equal(t, 512, provider.lastReq.MaxOutputTokens)
	require.InDelta(t, 0.1, provider.lastReq.Temperature, 0.0001)

	require.Len(t, provider.lastReq.Messages, 2)
	require.Equal(t, llm.RoleSystem, provider.lastReq.Messages[0].Role)
	require.Equal(t, llm.RoleUser, provider.lastReq.Messages[1].Role)

	user := provider.lastReq.Messages[1].Content
	require.Contains(t, user, "Pull request #42")
	require.Contains(t, user, "Title: Add feature X")
	require.Contains(t, user, "Author: alice")
	require.Contains(t, user, "feature/x -> main")
	require.Contains(t, user, "SIS-100")
	require.Contains(t, user, "internal/x/x.go")
	require.Contains(t, user, "migrations/000003_x.up.sql")
	require.Contains(t, user, "Lines changed: 145")
}

// ---------- error propagation ----------

func TestAnalyze_PropagatesLLMError(t *testing.T) {
	provider := &fakeProvider{err: llm.ErrUnauthorized}
	a, err := risk.New(risk.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrUnauthorized)
}

func TestAnalyze_MalformedJSON(t *testing.T) {
	provider := &fakeProvider{resp: &llm.Response{Content: "not json"}}
	a, err := risk.New(risk.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_DomainValidationFailure(t *testing.T) {
	// Level 9 is outside the domain RiskLevel range — NewRiskReport
	// must reject and the analyzer surfaces it as ErrInvalidResponse.
	provider := &fakeProvider{
		resp: &llm.Response{
			Content: `{"Level":9,"Factors":[],"Explanation":"x","Confidence":0.5}`,
		},
	}
	a, err := risk.New(risk.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_EmptyExplanationFails(t *testing.T) {
	// Domain invariant: explanation must not be empty.
	provider := &fakeProvider{
		resp: &llm.Response{
			Content: `{"Level":2,"Factors":[],"Explanation":"","Confidence":0.5}`,
		},
	}
	a, err := risk.New(risk.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_NilPullRequest(t *testing.T) {
	a, err := risk.New(risk.Config{Provider: &fakeProvider{}, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), nil)
	require.Error(t, err)
}

// ---------- corner cases (audit pass) ----------

func TestNew_RequiresSchemaName(t *testing.T) {
	spec := &llm.StructuredOutputSpec{Schema: []byte(`{"type":"object"}`)}
	_, err := risk.New(risk.Config{Provider: &fakeProvider{}, Schema: spec})
	require.Error(t, err)
}

func TestNew_RequiresSchemaBytes(t *testing.T) {
	spec := &llm.StructuredOutputSpec{Name: "risk_report"}
	_, err := risk.New(risk.Config{Provider: &fakeProvider{}, Schema: spec})
	require.Error(t, err)
}

// Nil response from a buggy provider must not panic; surfaces as
// ErrInvalidResponse.
func TestAnalyze_NilProviderResponse(t *testing.T) {
	provider := &fakeProvider{resp: nil, err: nil}
	a, err := risk.New(risk.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

// Some providers ignore strict mode and wrap JSON in a code fence.
// The decoder strips it transparently.
func TestAnalyze_StripsCodeFence(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"fenced with json tag", "```json\n{\"Level\":2,\"Factors\":[],\"Explanation\":\"ok\",\"Confidence\":0.5}\n```"},
		{"fenced without tag", "```\n{\"Level\":2,\"Factors\":[],\"Explanation\":\"ok\",\"Confidence\":0.5}\n```"},
		{"fenced with surrounding whitespace", "\n\n```json\n{\"Level\":2,\"Factors\":[],\"Explanation\":\"ok\",\"Confidence\":0.5}\n```\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider := &fakeProvider{resp: &llm.Response{Content: tc.content}}
			a, err := risk.New(risk.Config{Provider: provider, Schema: newSchema()})
			require.NoError(t, err)

			res, err := a.Analyze(context.Background(), newOpenPR(t))
			require.NoError(t, err)
			rr := res.Payload().(analysis.RiskReport)
			require.Equal(t, analysis.RiskMedium, rr.Level)
			require.Equal(t, "ok", rr.Explanation)
		})
	}
}

func TestAnalyze_PropagatesContextCancellation(t *testing.T) {
	provider := &fakeProvider{err: context.Canceled}
	a, err := risk.New(risk.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, context.Canceled)
}

func TestAnalyze_PropagatesContentFiltered(t *testing.T) {
	provider := &fakeProvider{err: llm.ErrContentFiltered}
	a, err := risk.New(risk.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrContentFiltered)
}

func TestAnalyze_PropagatesRateLimited(t *testing.T) {
	provider := &fakeProvider{err: llm.ErrRateLimited}
	a, err := risk.New(risk.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrRateLimited)
}

// A PR with no files / commits / reviewers / labels / tickets still
// produces a valid prompt — every optional section must be guarded.
func TestAnalyze_EmptyPRPromptRendersCleanly(t *testing.T) {
	author, err := shared.NewAuthor("bob", "", "")
	require.NoError(t, err)
	pr, err := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: shared.NewRepositoryID(),
		Number:       1,
		Title:        "Empty PR",
		Author:       author,
		SourceBranch: shared.MustBranch("a"),
		TargetBranch: shared.MustBranch("b"),
	}, shared.FixedClock{T: fixedTime})
	require.NoError(t, err)

	provider := &fakeProvider{
		resp: &llm.Response{Content: `{"Level":1,"Factors":[],"Explanation":"trivial","Confidence":0.9}`},
	}
	a, err := risk.New(risk.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), pr)
	require.NoError(t, err)

	u := provider.lastReq.Messages[1].Content
	require.Contains(t, u, "Pull request #1")
	require.Contains(t, u, "Title: Empty PR")
	require.Contains(t, u, "Lines changed: 0")
	require.NotContains(t, u, "Files (")
	require.NotContains(t, u, "Commits (")
	require.NotContains(t, u, "Reviewers:")
	require.NotContains(t, u, "Labels:")
	require.NotContains(t, u, "Tickets:")
	require.NotContains(t, u, "Description:")
}

func TestAnalyze_TruncatesLongDescription(t *testing.T) {
	long := strings.Repeat("x", 2000)
	author, err := shared.NewAuthor("bob", "", "")
	require.NoError(t, err)
	pr, err := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: shared.NewRepositoryID(),
		Number:       1,
		Title:        "Long desc",
		Description:  long,
		Author:       author,
		SourceBranch: shared.MustBranch("a"),
		TargetBranch: shared.MustBranch("b"),
	}, shared.FixedClock{T: fixedTime})
	require.NoError(t, err)

	provider := &fakeProvider{
		resp: &llm.Response{Content: `{"Level":1,"Factors":[],"Explanation":"ok","Confidence":0.5}`},
	}
	a, err := risk.New(risk.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), pr)
	require.NoError(t, err)

	u := provider.lastReq.Messages[1].Content
	require.Contains(t, u, "…", "expected truncation ellipsis")
	// Full 2000-rune description must not appear verbatim.
	require.NotContains(t, u, strings.Repeat("x", 1501))
}

func TestAnalyze_TruncatesLongFileAndCommitLists(t *testing.T) {
	author, err := shared.NewAuthor("bob", "", "")
	require.NoError(t, err)
	pr, err := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: shared.NewRepositoryID(),
		Number:       1,
		Title:        "Big PR",
		Author:       author,
		SourceBranch: shared.MustBranch("a"),
		TargetBranch: shared.MustBranch("b"),
	}, shared.FixedClock{T: fixedTime})
	require.NoError(t, err)

	for i := 0; i < 60; i++ {
		require.NoError(t, pr.AttachFile(pullrequest.ChangedFile{
			Path: "f" + itoa(i) + ".go", Status: pullrequest.FileModified, Additions: 1, Deletions: 1,
		}))
	}
	for i := 0; i < 15; i++ {
		sha, err := shared.NewSHA(strings.Repeat("a", 7) + itoa(i))
		require.NoError(t, err)
		require.NoError(t, pr.AttachCommit(pullrequest.Commit{
			SHA: sha, Message: "commit " + itoa(i), Author: author, Committed: fixedTime,
		}))
	}

	provider := &fakeProvider{
		resp: &llm.Response{Content: `{"Level":2,"Factors":["many_files"],"Explanation":"ok","Confidence":0.6}`},
	}
	a, err := risk.New(risk.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), pr)
	require.NoError(t, err)

	u := provider.lastReq.Messages[1].Content
	require.Contains(t, u, "Files (60 total, showing 50)")
	require.Contains(t, u, "Commits (15 total, showing 10)")
	// 51st file (index 50) must not appear.
	require.NotContains(t, u, "f50.go")
	// 11th commit (index 10) must not appear.
	require.NotContains(t, u, "commit 10")
}

// Tiny strconv-free itoa for test labels (mirrors the domain helper).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
