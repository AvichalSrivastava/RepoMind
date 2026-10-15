package qa_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/repomind/repomind/internal/application/analysis/qa"
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
		Name:   "qa_recommendations",
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

// happyContent is a single source of truth for "the model returns a
// well-formed payload" used across multiple tests.
const happyContent = `{
  "Summary": "Touches the x module and adds a schema migration. Focus regression on tenant isolation and migration rollback.",
  "RegressionAreas": ["tenant isolation", "migration rollback"],
  "ImpactedModules": ["internal/x"],
  "Scenarios": [
    {"Title": "Verify tenant isolation under the new x flow", "Description": "Run the multi-tenant fixture and assert no cross-tenant reads.", "Priority": 3, "Module": "internal/x"},
    {"Title": "Smoke the up/down migration on a copy of staging", "Description": "Apply 000003_x.up.sql then immediately the down; assert schema parity.", "Priority": 4, "Module": "migrations"}
  ]
}`

// ---------- constructor ----------

func TestNew_RequiresProvider(t *testing.T) {
	_, err := qa.New(qa.Config{Schema: newSchema()})
	require.Error(t, err)
}

func TestNew_RequiresSchema(t *testing.T) {
	_, err := qa.New(qa.Config{Provider: &fakeProvider{}})
	require.Error(t, err)
}

func TestNew_RequiresSchemaBytes(t *testing.T) {
	spec := &llm.StructuredOutputSpec{Name: "qa_recommendations"}
	_, err := qa.New(qa.Config{Provider: &fakeProvider{}, Schema: spec})
	require.Error(t, err)
}

func TestNew_RequiresSchemaName(t *testing.T) {
	spec := &llm.StructuredOutputSpec{Schema: []byte(`{"type":"object"}`)}
	_, err := qa.New(qa.Config{Provider: &fakeProvider{}, Schema: spec})
	require.Error(t, err)
}

func TestNew_DefaultsClock(t *testing.T) {
	a, err := qa.New(qa.Config{Provider: &fakeProvider{}, Schema: newSchema()})
	require.NoError(t, err)
	require.NotNil(t, a)
}

// ---------- happy path ----------

func TestAnalyze_HappyPath(t *testing.T) {
	provider := &fakeProvider{
		resp: &llm.Response{
			Content: happyContent,
			Model:   "gpt-4o-mini-2024-07-18",
			Usage:   llm.Usage{PromptTokens: 100, CompletionTokens: 80, TotalTokens: 180},
		},
	}
	a, err := qa.New(qa.Config{
		Provider: provider,
		Schema:   newSchema(),
		Clock:    shared.FixedClock{T: fixedTime},
	})
	require.NoError(t, err)

	pr := newOpenPR(t)
	result, err := a.Analyze(context.Background(), pr)
	require.NoError(t, err)

	require.Equal(t, analysis.KindQARecommendations, result.Kind())
	require.NotNil(t, result.Subject().PullRequestID)
	require.Equal(t, pr.ID(), *result.Subject().PullRequestID)
	require.Equal(t, qa.AnalyzerName, result.Analyzer().Name)
	require.Equal(t, qa.AnalyzerVersion, result.Analyzer().Version)
	require.Equal(t, qa.PromptVersion, result.Analyzer().PromptVersion)
	require.Equal(t, "gpt-4o-mini-2024-07-18", result.Model())
	require.Equal(t, 180, result.TokensUsed())
	require.Equal(t, fixedTime, result.GeneratedAt())

	qr, ok := result.Payload().(analysis.QARecommendations)
	require.True(t, ok)
	require.Equal(t,
		"Touches the x module and adds a schema migration. Focus regression on tenant isolation and migration rollback.",
		qr.Summary)
	require.ElementsMatch(t, []string{"tenant isolation", "migration rollback"}, qr.RegressionAreas)
	require.ElementsMatch(t, []string{"internal/x"}, qr.ImpactedModules)
	require.Len(t, qr.Scenarios, 2)
	require.Equal(t, analysis.PriorityP1, qr.Scenarios[0].Priority)
	require.Equal(t, analysis.PriorityP0, qr.Scenarios[1].Priority)
	require.Equal(t, "internal/x", qr.Scenarios[0].Module)
	require.Equal(t, "migrations", qr.Scenarios[1].Module)
}

func TestAnalyze_SendsCorrectRequest(t *testing.T) {
	provider := &fakeProvider{resp: &llm.Response{Content: happyContent}}
	schema := newSchema()
	a, err := qa.New(qa.Config{
		Provider:        provider,
		Schema:          schema,
		Clock:           shared.FixedClock{T: fixedTime},
		MaxOutputTokens: 1024,
		Temperature:     0.2,
	})
	require.NoError(t, err)

	pr := newOpenPR(t)
	_, err = a.Analyze(context.Background(), pr)
	require.NoError(t, err)

	require.Equal(t, llm.RoleQA, provider.lastReq.ModelRole)
	require.Same(t, schema, provider.lastReq.StructuredOutput)
	require.Equal(t, 1024, provider.lastReq.MaxOutputTokens)
	require.InDelta(t, 0.2, provider.lastReq.Temperature, 0.0001)

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
	a, err := qa.New(qa.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrUnauthorized)
}

func TestAnalyze_MalformedJSON(t *testing.T) {
	provider := &fakeProvider{resp: &llm.Response{Content: "not json"}}
	a, err := qa.New(qa.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_EmptySummaryFails(t *testing.T) {
	// Domain invariant: summary must not be empty.
	provider := &fakeProvider{
		resp: &llm.Response{
			Content: `{"Summary":"","RegressionAreas":[],"ImpactedModules":[],"Scenarios":[{"Title":"t","Description":"d","Priority":2,"Module":"m"}]}`,
		},
	}
	a, err := qa.New(qa.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_NoScenariosFails(t *testing.T) {
	// Domain invariant: at least one scenario.
	provider := &fakeProvider{
		resp: &llm.Response{
			Content: `{"Summary":"x","RegressionAreas":[],"ImpactedModules":[],"Scenarios":[]}`,
		},
	}
	a, err := qa.New(qa.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_EmptyScenarioTitleFails(t *testing.T) {
	provider := &fakeProvider{
		resp: &llm.Response{
			Content: `{"Summary":"x","RegressionAreas":[],"ImpactedModules":[],"Scenarios":[{"Title":"","Description":"d","Priority":2,"Module":"m"}]}`,
		},
	}
	a, err := qa.New(qa.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_OutOfRangePriorityFails(t *testing.T) {
	// Priority 9 is outside P3..P0 — NewQARecommendations must reject.
	provider := &fakeProvider{
		resp: &llm.Response{
			Content: `{"Summary":"x","RegressionAreas":[],"ImpactedModules":[],"Scenarios":[{"Title":"t","Description":"d","Priority":9,"Module":"m"}]}`,
		},
	}
	a, err := qa.New(qa.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestAnalyze_NilPullRequest(t *testing.T) {
	a, err := qa.New(qa.Config{Provider: &fakeProvider{}, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), nil)
	require.Error(t, err)
}

// ---------- corner cases ----------

// Nil response from a buggy provider must not panic; surfaces as
// ErrInvalidResponse.
func TestAnalyze_NilProviderResponse(t *testing.T) {
	provider := &fakeProvider{resp: nil, err: nil}
	a, err := qa.New(qa.Config{Provider: provider, Schema: newSchema()})
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
		{"fenced with json tag", "```json\n" + happyContent + "\n```"},
		{"fenced without tag", "```\n" + happyContent + "\n```"},
		{"fenced with surrounding whitespace", "\n\n```json\n" + happyContent + "\n```\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider := &fakeProvider{resp: &llm.Response{Content: tc.content}}
			a, err := qa.New(qa.Config{Provider: provider, Schema: newSchema()})
			require.NoError(t, err)

			res, err := a.Analyze(context.Background(), newOpenPR(t))
			require.NoError(t, err)
			qr := res.Payload().(analysis.QARecommendations)
			require.Len(t, qr.Scenarios, 2)
		})
	}
}

func TestAnalyze_PropagatesContextCancellation(t *testing.T) {
	provider := &fakeProvider{err: context.Canceled}
	a, err := qa.New(qa.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, context.Canceled)
}

func TestAnalyze_PropagatesContentFiltered(t *testing.T) {
	provider := &fakeProvider{err: llm.ErrContentFiltered}
	a, err := qa.New(qa.Config{Provider: provider, Schema: newSchema()})
	require.NoError(t, err)

	_, err = a.Analyze(context.Background(), newOpenPR(t))
	require.ErrorIs(t, err, llm.ErrContentFiltered)
}
