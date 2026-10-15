package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/repomind/repomind/internal/application/persistence"
)

// fakeEnqueuerForHandler is a tiny in-memory Enqueuer used by the
// webhook handler tests. The orchestration package's `enqueuer_test.go`
// already has a `fakeOutbox` for testing the enqueuer itself, but the
// HANDLER under test depends on the Enqueuer interface, not on the
// outbox port.
type fakeEnqueuerForHandler struct {
	mu            sync.Mutex
	webhookCalls  []GithubWebhookInput
	releaseCalls  []ReleaseNotesRequestInput
	prAnalysCalls []PRAnalysisRequestInput
	resp          EnqueueResult
	errPRAnalysis error
}

func (f *fakeEnqueuerForHandler) EnqueueGithubWebhook(
	_ context.Context, in GithubWebhookInput,
) (*EnqueueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.webhookCalls = append(f.webhookCalls, in)
	return &EnqueueResult{OutboxID: uuid.New()}, nil
}

func (f *fakeEnqueuerForHandler) EnqueueReleaseNotesRequest(
	_ context.Context, in ReleaseNotesRequestInput,
) (*EnqueueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls = append(f.releaseCalls, in)
	return &EnqueueResult{OutboxID: uuid.New()}, nil
}

func (f *fakeEnqueuerForHandler) EnqueuePRAnalysisRequest(
	_ context.Context, in PRAnalysisRequestInput,
) (*EnqueueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errPRAnalysis != nil {
		return nil, f.errPRAnalysis
	}
	f.prAnalysCalls = append(f.prAnalysCalls, in)
	r := f.resp
	if r.OutboxID == uuid.Nil {
		r.OutboxID = uuid.New()
	}
	return &r, nil
}

func (f *fakeEnqueuerForHandler) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prAnalysCalls)
}

// helpers ---------------------------------------------------------------

// mkWebhookEvent returns a fully-formed outbox event with the given
// GitHub event type + raw payload body.
func mkWebhookEvent(t *testing.T, event string, rawBody string) persistence.OutboxEvent {
	t.Helper()
	wh := GithubWebhookPayload{
		DeliveryID: "delivery-" + uuid.NewString(),
		Event:      event,
		Repo:       RepoRef{Owner: "acme", Name: "thing"},
		RawPayload: json.RawMessage(rawBody),
	}
	payload, err := json.Marshal(wh)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return persistence.OutboxEvent{
		ID:        uuid.New(),
		EventType: string(EventGithubWebhookReceived),
		Payload:   payload,
	}
}

// prRaw returns a minimal valid pull_request webhook envelope JSON.
func prRaw(action string, number int, headSHA string) string {
	body, _ := json.Marshal(map[string]any{
		"action": action,
		"number": number,
		"pull_request": map[string]any{
			"number": number,
			"head":   map[string]any{"sha": headSHA},
		},
	})
	return string(body)
}

// tests ----------------------------------------------------------------

func TestNewWebhookHandler_RejectsNilDeps(t *testing.T) {
	if _, err := NewWebhookHandler(nil, discardLogger(), nil); err == nil {
		t.Error("nil enqueuer: want error")
	}
	if _, err := NewWebhookHandler(&fakeEnqueuerForHandler{}, nil, nil); err == nil {
		t.Error("nil logger: want error")
	}
}

func TestWebhookHandler_PRAnalysableAction_Enqueues(t *testing.T) {
	cases := []string{"opened", "synchronize", "reopened", "ready_for_review"}
	for _, action := range cases {
		t.Run(action, func(t *testing.T) {
			enq := &fakeEnqueuerForHandler{}
			h, err := NewWebhookHandler(enq, discardLogger(), nil)
			if err != nil {
				t.Fatalf("ctor: %v", err)
			}
			evt := mkWebhookEvent(t, "pull_request", prRaw(action, 42, "deadbeef"))

			if err := h.Handle(context.Background(), evt); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := enq.callCount(); got != 1 {
				t.Fatalf("EnqueuePRAnalysisRequest calls: got %d want 1", got)
			}
			call := enq.prAnalysCalls[0]
			if call.Action != action {
				t.Errorf("Action: got %q want %q", call.Action, action)
			}
			if call.Number != 42 {
				t.Errorf("Number: got %d want 42", call.Number)
			}
			if call.HeadSHA != "deadbeef" {
				t.Errorf("HeadSHA: got %q want %q", call.HeadSHA, "deadbeef")
			}
			if call.Repo.Owner != "acme" || call.Repo.Name != "thing" {
				t.Errorf("Repo: got %+v want acme/thing", call.Repo)
			}
		})
	}
}

func TestWebhookHandler_PRNonAnalysableAction_NoOp(t *testing.T) {
	cases := []string{"closed", "edited", "labeled", "review_requested"}
	for _, action := range cases {
		t.Run(action, func(t *testing.T) {
			enq := &fakeEnqueuerForHandler{}
			h, _ := NewWebhookHandler(enq, discardLogger(), nil)
			evt := mkWebhookEvent(t, "pull_request", prRaw(action, 1, "abc"))

			if err := h.Handle(context.Background(), evt); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := enq.callCount(); got != 0 {
				t.Errorf("EnqueuePRAnalysisRequest calls: got %d want 0", got)
			}
		})
	}
}

func TestWebhookHandler_NonPREvent_NoOp(t *testing.T) {
	cases := []string{"ping", "push", "release", "create"}
	for _, event := range cases {
		t.Run(event, func(t *testing.T) {
			enq := &fakeEnqueuerForHandler{}
			h, _ := NewWebhookHandler(enq, discardLogger(), nil)
			// Body is irrelevant for non-PR events; handler ignores it.
			evt := mkWebhookEvent(t, event, `{"zen":"ok"}`)

			if err := h.Handle(context.Background(), evt); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := enq.callCount(); got != 0 {
				t.Errorf("EnqueuePRAnalysisRequest calls: got %d want 0", got)
			}
		})
	}
}

func TestWebhookHandler_BadPayload_ReturnsError(t *testing.T) {
	enq := &fakeEnqueuerForHandler{}
	h, _ := NewWebhookHandler(enq, discardLogger(), nil)
	evt := persistence.OutboxEvent{
		ID:        uuid.New(),
		EventType: string(EventGithubWebhookReceived),
		Payload:   []byte("{not json"),
	}
	if err := h.Handle(context.Background(), evt); err == nil {
		t.Error("want decode error, got nil")
	}
}

func TestWebhookHandler_BadRawPayload_ReturnsError(t *testing.T) {
	enq := &fakeEnqueuerForHandler{}
	h, _ := NewWebhookHandler(enq, discardLogger(), nil)
	// Build the outer payload by hand so we can embed an invalid
	// raw_payload (json.Marshal would reject it via RawMessage validation).
	body := []byte(`{
		"delivery_id":"d1",
		"event":"pull_request",
		"repo":{"owner":"acme","name":"thing"},
		"raw_payload":"not actually json"
	}`)
	evt := persistence.OutboxEvent{
		ID:        uuid.New(),
		EventType: string(EventGithubWebhookReceived),
		Payload:   body,
	}
	if err := h.Handle(context.Background(), evt); err == nil {
		t.Error("want decode error, got nil")
	}
}

func TestWebhookHandler_MissingHeadSHA_DropsQuietly(t *testing.T) {
	// Analysable action but no head SHA -- log + return nil so we
	// don't retry-loop on a malformed-but-passed-HMAC body.
	enq := &fakeEnqueuerForHandler{}
	h, _ := NewWebhookHandler(enq, discardLogger(), nil)
	evt := mkWebhookEvent(t, "pull_request", prRaw("opened", 7, ""))
	if err := h.Handle(context.Background(), evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := enq.callCount(); got != 0 {
		t.Errorf("EnqueuePRAnalysisRequest calls: got %d want 0", got)
	}
}

func TestWebhookHandler_UnexpectedEventType_ReturnsError(t *testing.T) {
	enq := &fakeEnqueuerForHandler{}
	h, _ := NewWebhookHandler(enq, discardLogger(), nil)
	evt := persistence.OutboxEvent{
		ID:        uuid.New(),
		EventType: "wrong.event.type",
		Payload:   []byte("{}"),
	}
	if err := h.Handle(context.Background(), evt); err == nil {
		t.Error("want unexpected-type error, got nil")
	}
}

func TestWebhookHandler_EnqueueFailureBubblesUp(t *testing.T) {
	enq := &fakeEnqueuerForHandler{errPRAnalysis: errors.New("postgres down")}
	h, _ := NewWebhookHandler(enq, discardLogger(), nil)
	evt := mkWebhookEvent(t, "pull_request", prRaw("opened", 1, "sha1"))
	err := h.Handle(context.Background(), evt)
	if err == nil {
		t.Error("want error, got nil")
	}
}

// prRawWithBase is like prRaw but also embeds the base.ref for the
// policy defense-in-depth test below.
func prRawWithBase(action string, number int, headSHA, baseRef string) string {
	body, _ := json.Marshal(map[string]any{
		"action": action,
		"number": number,
		"pull_request": map[string]any{
			"number": number,
			"head":   map[string]any{"sha": headSHA},
			"base":   map[string]any{"ref": baseRef},
		},
	})
	return string(body)
}

// fixedPolicy is a minimal apigithub.BranchPolicy used by the
// integration tests below — accepts only branches in `allow`.
type fixedPolicy struct{ allow map[string]struct{} }

func newFixedPolicy(branches ...string) *fixedPolicy {
	m := make(map[string]struct{}, len(branches))
	for _, b := range branches {
		m[b] = struct{}{}
	}
	return &fixedPolicy{allow: m}
}
func (p *fixedPolicy) Accept(_, _ string, branch string) bool {
	_, ok := p.allow[branch]
	return ok
}

func TestWebhookHandler_BranchPolicy_AcceptedBranch_Enqueues(t *testing.T) {
	enq := &fakeEnqueuerForHandler{}
	policy := newFixedPolicy("qa")
	h, _ := NewWebhookHandler(enq, discardLogger(), policy)
	evt := mkWebhookEvent(t, "pull_request",
		prRawWithBase("opened", 7, "sha7", "qa"))

	if err := h.Handle(context.Background(), evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := enq.callCount(); got != 1 {
		t.Errorf("EnqueuePRAnalysisRequest calls: got %d want 1", got)
	}
}

func TestWebhookHandler_BranchPolicy_RejectedBranch_NoEnqueue(t *testing.T) {
	enq := &fakeEnqueuerForHandler{}
	policy := newFixedPolicy("qa") // accepts only "qa"
	h, _ := NewWebhookHandler(enq, discardLogger(), policy)
	evt := mkWebhookEvent(t, "pull_request",
		prRawWithBase("opened", 7, "sha7", "main")) // not allowed

	if err := h.Handle(context.Background(), evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := enq.callCount(); got != 0 {
		t.Errorf("EnqueuePRAnalysisRequest calls: got %d want 0 (policy reject)", got)
	}
}

func TestWebhookHandler_BranchPolicy_EmptyBaseRef_Enqueues(t *testing.T) {
	// When the webhook payload lacks base.ref we can't decide; fall
	// through to enqueue rather than silently drop.
	enq := &fakeEnqueuerForHandler{}
	policy := newFixedPolicy("qa")
	h, _ := NewWebhookHandler(enq, discardLogger(), policy)
	evt := mkWebhookEvent(t, "pull_request",
		prRawWithBase("opened", 7, "sha7", "")) // empty base

	if err := h.Handle(context.Background(), evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := enq.callCount(); got != 1 {
		t.Errorf("EnqueuePRAnalysisRequest calls: got %d want 1 (can't decide -> accept)", got)
	}
}
