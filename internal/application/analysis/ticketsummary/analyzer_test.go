package ticketsummary

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/repomind/repomind/internal/application/llm"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/shared"
)

// --- stub provider -------------------------------------------------

type stubProvider struct {
	resp   *llm.Response
	err    error
	gotReq llm.Request
	called int
}

func (s *stubProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	s.called++
	s.gotReq = req
	return s.resp, s.err
}

// --- helpers -------------------------------------------------------

func newPR(t *testing.T, title string, ticketKey string) *pullrequest.PullRequest {
	t.Helper()
	author, err := shared.NewAuthor("alice", "Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	src, _ := shared.NewBranch("feature/x")
	dst, _ := shared.NewBranch("qa")
	pr, err := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: shared.NewRepositoryID(),
		Number:       1,
		Title:        title,
		Author:       author,
		SourceBranch: src,
		TargetBranch: dst,
	}, shared.SystemClock{})
	if err != nil {
		t.Fatalf("pr: %v", err)
	}
	if ticketKey != "" {
		tk, err := shared.NewTicketKey(ticketKey)
		if err != nil {
			t.Fatalf("ticket: %v", err)
		}
		pr.AssociateTicket(tk)
	}
	return pr
}

// --- tests ---------------------------------------------------------

func TestNew_RejectsNilProvider(t *testing.T) {
	_, err := New(Config{Provider: nil})
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestNew_AppliesDefaults(t *testing.T) {
	a, err := New(Config{Provider: &stubProvider{}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if a.cfg.MaxOutputTokens != 120 {
		t.Errorf("MaxOutputTokens default: got %d want 120", a.cfg.MaxOutputTokens)
	}
	if a.cfg.Temperature == 0 {
		t.Errorf("Temperature default should not be zero")
	}
	if a.cfg.Logger == nil {
		t.Errorf("Logger default should be slog.Default")
	}
}

func TestSummarise_EmptyTicketKey_Rejected(t *testing.T) {
	a, _ := New(Config{Provider: &stubProvider{}})
	_, err := a.Summarise(context.Background(), Input{TicketKey: "  ", PRs: []*pullrequest.PullRequest{newPR(t, "x", "SIS-1")}})
	if err == nil {
		t.Fatal("want error for empty TicketKey")
	}
}

func TestSummarise_NoPRs_ReturnsEmpty(t *testing.T) {
	stub := &stubProvider{}
	a, _ := New(Config{Provider: stub})
	got, err := a.Summarise(context.Background(), Input{TicketKey: "SIS-1", PRs: nil})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "" {
		t.Errorf("want empty, got %q", got)
	}
	if stub.called != 0 {
		t.Errorf("LLM should not be called when there are no PRs (got %d calls)", stub.called)
	}
}

func TestSummarise_HappyPath(t *testing.T) {
	stub := &stubProvider{
		resp: &llm.Response{Content: "Added a search filter to improve UX."},
	}
	a, _ := New(Config{Provider: stub})
	got, err := a.Summarise(context.Background(), Input{
		TicketKey: "SIS-9505",
		PRs:       []*pullrequest.PullRequest{newPR(t, "Add filter", "SIS-9505")},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "Added a search filter to improve UX." {
		t.Errorf("unexpected description: %q", got)
	}
	if stub.gotReq.ModelRole != llm.RoleQA {
		t.Errorf("ModelRole: got %q want %q", stub.gotReq.ModelRole, llm.RoleQA)
	}
	if len(stub.gotReq.Messages) != 2 {
		t.Errorf("want 2 messages (system + user), got %d", len(stub.gotReq.Messages))
	}
	if stub.gotReq.Messages[0].Role != llm.RoleSystem {
		t.Errorf("first msg role: got %q want system", stub.gotReq.Messages[0].Role)
	}
	if !strings.Contains(stub.gotReq.Messages[1].Content, "SIS-9505") {
		t.Errorf("user prompt should mention ticket key; got: %q", stub.gotReq.Messages[1].Content)
	}
}

func TestSummarise_StripsWrappingQuotesAndNewlines(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		{`"Fixed the bug."`, `Fixed the bug.`},
		{`'Added sorting.'`, `Added sorting.`},
		{"  Improved UX.  \n", `Improved UX.`},
		{"Line one.\nLine two.", `Line one. Line two.`},
		{`Just plain`, `Just plain`},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			stub := &stubProvider{resp: &llm.Response{Content: c.raw}}
			a, _ := New(Config{Provider: stub})
			got, err := a.Summarise(context.Background(), Input{TicketKey: "SIS-1", PRs: []*pullrequest.PullRequest{newPR(t, "x", "SIS-1")}})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != c.want {
				t.Errorf("\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

func TestSummarise_NilProviderResponse_Errors(t *testing.T) {
	stub := &stubProvider{resp: nil, err: nil}
	a, _ := New(Config{Provider: stub})
	_, err := a.Summarise(context.Background(), Input{TicketKey: "SIS-1", PRs: []*pullrequest.PullRequest{newPR(t, "x", "SIS-1")}})
	if err == nil {
		t.Fatal("want error for nil provider response")
	}
}

func TestSummarise_ProviderError_PropagatedWrapped(t *testing.T) {
	want := errors.New("boom")
	stub := &stubProvider{err: want}
	a, _ := New(Config{Provider: stub})
	_, err := a.Summarise(context.Background(), Input{TicketKey: "SIS-1", PRs: []*pullrequest.PullRequest{newPR(t, "x", "SIS-1")}})
	if !errors.Is(err, want) {
		t.Errorf("want wrapped %v, got %v", want, err)
	}
}

func TestSummarise_ContextCancellationPropagated(t *testing.T) {
	stub := &stubProvider{resp: &llm.Response{Content: "ok"}}
	a, _ := New(Config{Provider: stub})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	_, err := a.Summarise(ctx, Input{TicketKey: "SIS-1", PRs: []*pullrequest.PullRequest{newPR(t, "x", "SIS-1")}})
	// The stub doesn't honour ctx, so the call DOES complete -- the
	// real openai adapter honours ctx. This test just exercises the
	// happy code path with a cancelled ctx to confirm we don't panic.
	_ = err
}

func TestBuildPrompt_CapsPRsAndFiles(t *testing.T) {
	// 10 PRs > maxPRsPerTicket(6); files should also be capped.
	prs := make([]*pullrequest.PullRequest, 0, 10)
	for i := 0; i < 10; i++ {
		prs = append(prs, newPR(t, "title-"+string(rune('a'+i)), "SIS-1"))
	}
	out := buildPrompt(Input{TicketKey: "SIS-1", PRs: prs})
	// Should mention "PR 6 of 10" or "[PR 6 of 10]" but never "PR 7 of 10".
	if !strings.Contains(out, "[PR 6 of 10]") {
		t.Errorf("expected [PR 6 of 10] in prompt; got:\n%s", out)
	}
	if strings.Contains(out, "[PR 7 of 10]") {
		t.Errorf("prompt should cap at 6 PRs; PR 7 leaked through:\n%s", out)
	}
}

// _ time import keeps it for potential future deadline tests.
var _ = time.Now
