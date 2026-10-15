package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	apijira "github.com/repomind/repomind/internal/application/jira"
	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/repomind/repomind/internal/domain/sprint"
)

// ---- fakes ---------------------------------------------------------

type fakeWorkerState struct {
	mu     sync.Mutex
	byKey  map[string]json.RawMessage
	getErr error
}

func newFakeWorkerState() *fakeWorkerState {
	return &fakeWorkerState{byKey: map[string]json.RawMessage{}}
}
func (f *fakeWorkerState) Get(_ context.Context, key string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	v, ok := f.byKey[key]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return v, nil
}
func (f *fakeWorkerState) Set(_ context.Context, key string, value json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(json.RawMessage, len(value))
	copy(cp, value)
	f.byKey[key] = cp
	return nil
}

// fakeJiraSprints stubs ListSprints (the ticker uses only this method
// from the Jira port, plus inherits the no-op default fakeJira).
type fakeJiraSprints struct {
	fakeJira
	sprints []apijira.Sprint
	listErr error
}

func (f *fakeJiraSprints) ListSprints(_ context.Context, _ int64, _ apijira.ListSprintsOptions) (*apijira.SprintPage, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	cp := make([]apijira.Sprint, len(f.sprints))
	copy(cp, f.sprints)
	return &apijira.SprintPage{Items: cp}, nil
}

// fakeTickerEnqueuer captures EnqueueReleaseNotesRequest calls.
type fakeTickerEnqueuer struct {
	mu    sync.Mutex
	calls []ReleaseNotesRequestInput
	err   error
}

func (e *fakeTickerEnqueuer) EnqueueGithubWebhook(context.Context, GithubWebhookInput) (*EnqueueResult, error) {
	return &EnqueueResult{}, nil
}
func (e *fakeTickerEnqueuer) EnqueuePRAnalysisRequest(context.Context, PRAnalysisRequestInput) (*EnqueueResult, error) {
	return &EnqueueResult{}, nil
}
func (e *fakeTickerEnqueuer) EnqueueReleaseNotesRequest(_ context.Context, in ReleaseNotesRequestInput) (*EnqueueResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return nil, e.err
	}
	e.calls = append(e.calls, in)
	return &EnqueueResult{}, nil
}

// ---- helpers --------------------------------------------------------

// mkJiraSprint builds an apijira.Sprint DTO with sensible defaults.
func mkJiraSprint(id int64, name, state string, endsAt time.Time) apijira.Sprint {
	end := endsAt
	return apijira.Sprint{
		ID:        id,
		Name:      name,
		State:     state,
		StartDate: ptrTime(endsAt.Add(-14 * 24 * time.Hour)),
		EndDate:   &end,
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// buildTicker wires the ticker against the supplied fakes.
func buildTicker(t *testing.T, jira apijira.Client, sprints *fakeSprints, ws *fakeWorkerState, enq Enqueuer) *SprintTicker {
	t.Helper()
	stores := persistence.Stores{
		Sprints:     sprints,
		WorkerState: ws,
	}
	ti, err := NewSprintTicker(SprintTickerConfig{
		TeamName:         "Marvelous",
		BoardID:          187,
		SprintNamePrefix: "Marvelous_Sprint_",
		Interval:         time.Minute,
	}, SprintTickerDeps{
		Jira:     jira,
		Stores:   stores,
		UoW:      &trivialUoW{s: stores},
		Enqueuer: enq,
		Logger:   discardLogger(),
		Clock:    shared.SystemClock{},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	return ti
}

// readBookmark returns the saved tickerBookmark for the team's key.
func readBookmark(t *testing.T, ws *fakeWorkerState, team string) tickerBookmark {
	t.Helper()
	raw, err := ws.Get(context.Background(), "ticker:team:"+team)
	if err != nil {
		return tickerBookmark{}
	}
	var bm tickerBookmark
	if err := json.Unmarshal(raw, &bm); err != nil {
		t.Fatalf("unmarshal bookmark: %v", err)
	}
	return bm
}

// ---- tests ----------------------------------------------------------

func TestNewSprintTicker_RejectsBadConfig(t *testing.T) {
	stores := persistence.Stores{
		Sprints:     newFakeSprints(),
		WorkerState: newFakeWorkerState(),
	}
	full := SprintTickerDeps{
		Jira:     &fakeJiraSprints{},
		Stores:   stores,
		UoW:      &trivialUoW{s: stores},
		Enqueuer: &fakeTickerEnqueuer{},
		Logger:   discardLogger(),
	}
	good := SprintTickerConfig{
		TeamName: "x", BoardID: 1, SprintNamePrefix: "p", Interval: time.Second,
	}
	bad := []SprintTickerConfig{
		{BoardID: 1, SprintNamePrefix: "p", Interval: time.Second},    // no team
		{TeamName: "x", SprintNamePrefix: "p", Interval: time.Second}, // no board
		{TeamName: "x", BoardID: 1, Interval: time.Second},            // no prefix
		{TeamName: "x", BoardID: 1, SprintNamePrefix: "p"},            // no interval
	}
	for i, c := range bad {
		if _, err := NewSprintTicker(c, full); err == nil {
			t.Errorf("bad cfg %d: want error, got nil", i)
		}
	}
	if _, err := NewSprintTicker(good, full); err != nil {
		t.Fatalf("good cfg: %v", err)
	}
}

func TestSprintTicker_FirstTick_FiresActiveEnqueue(t *testing.T) {
	// First tick observes an active sprint. New semantics: every tick
	// where a sprint is active enqueues a release-notes request (with
	// a tick-bucketed dedup key). This is what makes per-sprint pages
	// materialise as soon as a sprint becomes active on Jira.
	ws := newFakeWorkerState()
	enq := &fakeTickerEnqueuer{}
	sprints := newFakeSprints()
	_ = sprints.Save(context.Background(), mkSprint(t, 187, sprint.StateActive))
	jira := &fakeJiraSprints{sprints: []apijira.Sprint{
		mkJiraSprint(187, "Marvelous_Sprint_187", "active", time.Now().Add(7*24*time.Hour)),
	}}
	tk := buildTicker(t, jira, sprints, ws, enq)

	tk.tickOnce(context.Background())

	if len(enq.calls) != 1 {
		t.Fatalf("enqueue calls: got %d want 1 (active-sprint refresh)", len(enq.calls))
	}
	if got := enq.calls[0].Reason; got != "ticker-active" {
		t.Errorf("reason: got %q want ticker-active", got)
	}
	if got := enq.calls[0].IdempotencyKey; !strings.Contains(got, ":active_tick=") {
		t.Errorf("idempotency key must contain :active_tick= bucket; got %q", got)
	}
	bm := readBookmark(t, ws, "marvelous")
	if bm.LastActiveJiraSprintID != 187 {
		t.Errorf("bookmark active id: got %d want 187", bm.LastActiveJiraSprintID)
	}
	if bm.LastActiveName != "Marvelous_Sprint_187" {
		t.Errorf("bookmark name: got %q want %q", bm.LastActiveName, "Marvelous_Sprint_187")
	}
}

func TestSprintTicker_ActiveToClosed_FiresEnqueue(t *testing.T) {
	// Seed bookmark: sprint 187 was active.
	ws := newFakeWorkerState()
	prior := tickerBookmark{LastActiveJiraSprintID: 187, LastActiveName: "Marvelous_Sprint_187", LastObservedAt: time.Now()}
	priorBytes, _ := json.Marshal(prior)
	_ = ws.Set(context.Background(), "ticker:team:marvelous", priorBytes)

	// Seed sprint 187 in DB so FindByJiraID succeeds.
	sprints := newFakeSprints()
	mirrored := mkSprint(t, 187, sprint.StateActive)

	_ = sprints.Save(context.Background(), mirrored)

	enq := &fakeTickerEnqueuer{}
	jira := &fakeJiraSprints{sprints: []apijira.Sprint{
		mkJiraSprint(187, "Marvelous_Sprint_187", "closed", time.Now().Add(-1*time.Hour)),
	}}
	tk := buildTicker(t, jira, sprints, ws, enq)

	tk.tickOnce(context.Background())

	if len(enq.calls) != 1 {
		t.Fatalf("enqueue calls: got %d want 1 (only closed; no active sprint left)", len(enq.calls))
	}
	call := enq.calls[0]
	if call.Reason != "ticker-closed" {
		t.Errorf("reason: got %q want ticker-closed", call.Reason)
	}
	if !strings.Contains(call.IdempotencyKey, ":closed=") {
		t.Errorf("idempotency key must contain :closed=; got %q", call.IdempotencyKey)
	}
	// Bookmark now reflects "no active" (because 187 is closed and no other active sprint).
	bm := readBookmark(t, ws, "marvelous")
	if bm.LastActiveJiraSprintID != 0 {
		t.Errorf("bookmark after closure: got id=%d want 0", bm.LastActiveJiraSprintID)
	}
}

func TestSprintTicker_ActiveSprintUnchanged_FiresActiveTickEveryTick(t *testing.T) {
	// Bookmark says 187 active; Jira still says 187 active. New
	// semantics: fire on EVERY tick while a sprint is active, gated
	// only by the tick-bucketed dedup key at the outbox layer. The
	// handler's L3 hash short-circuit keeps repeat fires cheap.
	ws := newFakeWorkerState()
	prior := tickerBookmark{LastActiveJiraSprintID: 187, LastActiveName: "Marvelous_Sprint_187"}
	priorBytes, _ := json.Marshal(prior)
	_ = ws.Set(context.Background(), "ticker:team:marvelous", priorBytes)

	sprints := newFakeSprints()
	_ = sprints.Save(context.Background(), mkSprint(t, 187, sprint.StateActive))

	enq := &fakeTickerEnqueuer{}
	jira := &fakeJiraSprints{sprints: []apijira.Sprint{
		mkJiraSprint(187, "Marvelous_Sprint_187", "active", time.Now().Add(7*24*time.Hour)),
	}}
	tk := buildTicker(t, jira, sprints, ws, enq)

	tk.tickOnce(context.Background())

	if len(enq.calls) != 1 {
		t.Fatalf("enqueue calls: got %d want 1 (active-tick refresh)", len(enq.calls))
	}
	if got := enq.calls[0].Reason; got != "ticker-active" {
		t.Errorf("reason: got %q want ticker-active", got)
	}
}

func TestSprintTicker_ActiveToNewActive_FiresOldClosedAndNewActive(t *testing.T) {
	// Bookmark: 186 active. Jira: 186 closed AND 187 active. Both
	// enqueues fire on this tick:
	//   - 186 -> ticker-closed (seal the release notes)
	//   - 187 -> ticker-active (materialise the new sprint's page)
	ws := newFakeWorkerState()
	prior := tickerBookmark{LastActiveJiraSprintID: 186, LastActiveName: "Marvelous_Sprint_186"}
	priorBytes, _ := json.Marshal(prior)
	_ = ws.Set(context.Background(), "ticker:team:marvelous", priorBytes)

	sprints := newFakeSprints()
	_ = sprints.Save(context.Background(), mkSprint(t, 186, sprint.StateActive))
	_ = sprints.Save(context.Background(), mkSprint(t, 187, sprint.StateActive))

	enq := &fakeTickerEnqueuer{}
	jira := &fakeJiraSprints{sprints: []apijira.Sprint{
		mkJiraSprint(186, "Marvelous_Sprint_186", "closed", time.Now().Add(-1*time.Hour)),
		mkJiraSprint(187, "Marvelous_Sprint_187", "active", time.Now().Add(7*24*time.Hour)),
	}}
	tk := buildTicker(t, jira, sprints, ws, enq)

	tk.tickOnce(context.Background())

	if len(enq.calls) != 2 {
		t.Fatalf("enqueue calls: got %d want 2 (186 closed + 187 active)", len(enq.calls))
	}
	reasons := map[string]bool{enq.calls[0].Reason: true, enq.calls[1].Reason: true}
	if !reasons["ticker-closed"] || !reasons["ticker-active"] {
		t.Errorf("reasons: got %v want both ticker-closed and ticker-active", reasons)
	}
	bm := readBookmark(t, ws, "marvelous")
	if bm.LastActiveJiraSprintID != 187 {
		t.Errorf("bookmark should move to 187, got %d", bm.LastActiveJiraSprintID)
	}
}

func TestSprintTicker_JiraError_DoesNotUpdateBookmark(t *testing.T) {
	ws := newFakeWorkerState()
	enq := &fakeTickerEnqueuer{}
	jira := &fakeJiraSprints{listErr: errors.New("jira 500")}
	tk := buildTicker(t, jira, newFakeSprints(), ws, enq)

	tk.tickOnce(context.Background())

	if _, err := ws.Get(context.Background(), "ticker:team:marvelous"); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("bookmark should not be written on Jira failure; got err=%v", err)
	}
}

func TestSprintTicker_EnqueueError_DoesNotUpdateBookmark(t *testing.T) {
	ws := newFakeWorkerState()
	prior := tickerBookmark{LastActiveJiraSprintID: 187, LastActiveName: "Marvelous_Sprint_187"}
	priorBytes, _ := json.Marshal(prior)
	_ = ws.Set(context.Background(), "ticker:team:marvelous", priorBytes)

	sprints := newFakeSprints()
	mirrored := mkSprint(t, 187, sprint.StateActive)
	_ = sprints.Save(context.Background(), mirrored)

	enq := &fakeTickerEnqueuer{err: errors.New("postgres down")}
	jira := &fakeJiraSprints{sprints: []apijira.Sprint{
		mkJiraSprint(187, "Marvelous_Sprint_187", "closed", time.Now().Add(-1*time.Hour)),
	}}
	tk := buildTicker(t, jira, sprints, ws, enq)

	tk.tickOnce(context.Background())

	// Bookmark must remain unchanged so next tick retries the enqueue.
	bm := readBookmark(t, ws, "marvelous")
	if bm.LastActiveJiraSprintID != 187 {
		t.Errorf("bookmark should not be updated on enqueue failure; got id=%d", bm.LastActiveJiraSprintID)
	}
}

func TestSprintTicker_PrefixFiltersOtherTeams(t *testing.T) {
	// Two teams share the board. Our prefix is Marvelous_Sprint_;
	// Kang_Sprint_X is filtered out and must not influence state.
	ws := newFakeWorkerState()
	enq := &fakeTickerEnqueuer{}
	sprints := newFakeSprints()
	// Seed 187 so the active-tick enqueue (post-transition) can
	// resolve its local UUID via FindByJiraID.
	_ = sprints.Save(context.Background(), mkSprint(t, 187, sprint.StateActive))
	jira := &fakeJiraSprints{sprints: []apijira.Sprint{
		mkJiraSprint(900, "Kang_Sprint_900", "active", time.Now().Add(7*24*time.Hour)),
		mkJiraSprint(187, "Marvelous_Sprint_187", "active", time.Now().Add(7*24*time.Hour)),
	}}
	tk := buildTicker(t, jira, sprints, ws, enq)

	tk.tickOnce(context.Background())

	bm := readBookmark(t, ws, "marvelous")
	if bm.LastActiveJiraSprintID != 187 {
		t.Errorf("bookmark should reflect Marvelous-only sprint, got id=%d", bm.LastActiveJiraSprintID)
	}
	// Kang sprint must NOT have triggered any enqueue.
	for _, c := range enq.calls {
		if strings.Contains(c.IdempotencyKey, "kang") {
			t.Errorf("Kang sprint leaked into enqueue: %+v", c)
		}
	}
}

// sprintStateForTest is removed -- direct sprint.State is used.
