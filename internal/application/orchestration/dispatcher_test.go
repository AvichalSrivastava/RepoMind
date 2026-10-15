package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/repomind/repomind/internal/application/persistence"
)

// ---------- test helpers ----------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 4}))
}

// fakeDispatchOutbox is an in-memory persistence.Outbox just rich enough for
// the dispatcher's processOne / drainOnce code paths. Run() is not
// exercised here ??? it needs a real *pgxpool.Pool LISTEN conn and is
// covered by an integration test.
type fakeDispatchOutbox struct {
	mu      sync.Mutex
	events  []persistence.OutboxEvent
	calls   []string
	lastErr map[uuid.UUID]string
}

func newFakeDispatchOutbox(evts ...persistence.OutboxEvent) *fakeDispatchOutbox {
	cp := make([]persistence.OutboxEvent, len(evts))
	copy(cp, evts)
	return &fakeDispatchOutbox{
		events:  cp,
		lastErr: make(map[uuid.UUID]string),
	}
}

func (f *fakeDispatchOutbox) Enqueue(_ context.Context, _ persistence.OutboxEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Enqueue")
	return nil
}

func (f *fakeDispatchOutbox) EnqueueWithDedup(_ context.Context, _ persistence.OutboxEvent) (persistence.EnqueueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "EnqueueWithDedup")
	return persistence.EnqueueResult{}, nil
}

func (f *fakeDispatchOutbox) ListPending(_ context.Context, limit int) ([]persistence.OutboxEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "ListPending")
	// Return only events still 'pending' AND whose next_attempt_at is past.
	now := time.Now().UTC()
	out := make([]persistence.OutboxEvent, 0, len(f.events))
	for _, e := range f.events {
		if e.Status != persistence.OutboxPending {
			continue
		}
		if e.NextAttemptAt.After(now) {
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeDispatchOutbox) MarkDispatched(_ context.Context, id uuid.UUID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "MarkDispatched")
	for i := range f.events {
		if f.events[i].ID == id {
			f.events[i].Status = persistence.OutboxDispatched
			f.events[i].DispatchedAt = &at
			return nil
		}
	}
	return errors.New("not found")
}

func (f *fakeDispatchOutbox) MarkFailed(_ context.Context, id uuid.UUID, lastErr string, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "MarkFailed")
	f.lastErr[id] = lastErr
	for i := range f.events {
		if f.events[i].ID == id {
			f.events[i].Attempts++
			f.events[i].LastError = lastErr
			f.events[i].NextAttemptAt = next
			return nil
		}
	}
	return errors.New("not found")
}

func (f *fakeDispatchOutbox) MarkPermanentlyFailed(_ context.Context, id uuid.UUID, lastErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "MarkPermanentlyFailed")
	f.lastErr[id] = lastErr
	for i := range f.events {
		if f.events[i].ID == id {
			f.events[i].Status = persistence.OutboxFailed
			f.events[i].Attempts++
			f.events[i].LastError = lastErr
			return nil
		}
	}
	return nil // idempotent: row gone counts as failed
}

func (f *fakeDispatchOutbox) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

func (f *fakeDispatchOutbox) snapshot(id uuid.UUID) (persistence.OutboxEvent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.ID == id {
			return e, true
		}
	}
	return persistence.OutboxEvent{}, false
}

// validCfg returns a fully-populated DispatcherConfig for tests.
func validCfg() DispatcherConfig {
	return DispatcherConfig{
		PollInterval:   30 * time.Second,
		BatchSize:      8,
		MaxAttempts:    3,
		BaseBackoff:    10 * time.Second,
		HandlerTimeout: 1 * time.Second,
		ShutdownGrace:  1 * time.Second,
	}
}

// mkEvent returns a pending OutboxEvent ready for ListPending.
func mkEvent(eventType string, attempts int) persistence.OutboxEvent {
	return persistence.OutboxEvent{
		ID:            uuid.New(),
		AggregateType: "sprint",
		AggregateID:   uuid.New(),
		EventType:     eventType,
		Payload:       json.RawMessage(`{}`),
		Status:        persistence.OutboxPending,
		Attempts:      attempts,
		NextAttemptAt: time.Now().UTC().Add(-time.Minute), // due
		CreatedAt:     time.Now().UTC().Add(-time.Hour),
	}
}

// frozenClock returns a clock whose successive Now() calls advance by
// step. Useful for asserting MarkFailed's next_attempt_at.
func frozenClock(start time.Time, step time.Duration) func() time.Time {
	cur := start
	var mu sync.Mutex
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := cur
		cur = cur.Add(step)
		return t
	}
}

// ---------- construction tests ----------

func TestNewDispatcher_RejectsNilDeps(t *testing.T) {
	cfg := validCfg()
	pool := &pgxpool.Pool{} // non-nil zero is fine for construction validation
	ob := newFakeDispatchOutbox()
	hs := Handlers{"x": HandlerFunc(func(context.Context, persistence.OutboxEvent) error { return nil })}
	lg := discardLogger()

	cases := []struct {
		name string
		deps DispatcherDeps
		want string
	}{
		{"nil pool", DispatcherDeps{Outbox: ob, Handlers: hs, Logger: lg}, "Pool"},
		{"nil outbox", DispatcherDeps{Pool: pool, Handlers: hs, Logger: lg}, "Outbox"},
		{"nil logger", DispatcherDeps{Pool: pool, Outbox: ob, Handlers: hs}, "Logger"},
		{"empty handlers", DispatcherDeps{Pool: pool, Outbox: ob, Logger: lg, Handlers: Handlers{}}, "Handler"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewDispatcher(cfg, c.deps)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.want)
			}
		})
	}
}

func TestNewDispatcher_RejectsBadConfig(t *testing.T) {
	deps := DispatcherDeps{
		Pool:     &pgxpool.Pool{},
		Outbox:   newFakeDispatchOutbox(),
		Logger:   discardLogger(),
		Handlers: Handlers{"x": HandlerFunc(func(context.Context, persistence.OutboxEvent) error { return nil })},
	}
	bad := []DispatcherConfig{
		{}, // all zero
		func() DispatcherConfig { c := validCfg(); c.PollInterval = 0; return c }(),
		func() DispatcherConfig { c := validCfg(); c.BatchSize = 0; return c }(),
		func() DispatcherConfig { c := validCfg(); c.MaxAttempts = 0; return c }(),
		func() DispatcherConfig { c := validCfg(); c.BaseBackoff = 0; return c }(),
		func() DispatcherConfig { c := validCfg(); c.HandlerTimeout = 0; return c }(),
		func() DispatcherConfig { c := validCfg(); c.ShutdownGrace = 0; return c }(),
	}
	for i, cfg := range bad {
		if _, err := NewDispatcher(cfg, deps); err == nil {
			t.Errorf("case %d: want validation error, got nil", i)
		}
	}
}

func TestNewDispatcher_RejectsNilHandler(t *testing.T) {
	deps := DispatcherDeps{
		Pool:     &pgxpool.Pool{},
		Outbox:   newFakeDispatchOutbox(),
		Logger:   discardLogger(),
		Handlers: Handlers{"x": nil},
	}
	if _, err := NewDispatcher(validCfg(), deps); err == nil {
		t.Fatal("want error for nil handler, got nil")
	}
}

func TestRegisteredEventTypes_IsSorted(t *testing.T) {
	d, err := NewDispatcher(validCfg(), DispatcherDeps{
		Pool:   &pgxpool.Pool{},
		Outbox: newFakeDispatchOutbox(),
		Logger: discardLogger(),
		Handlers: Handlers{
			"c.event": HandlerFunc(func(context.Context, persistence.OutboxEvent) error { return nil }),
			"a.event": HandlerFunc(func(context.Context, persistence.OutboxEvent) error { return nil }),
			"b.event": HandlerFunc(func(context.Context, persistence.OutboxEvent) error { return nil }),
		},
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	got := d.RegisteredEventTypes()
	want := []string{"a.event", "b.event", "c.event"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

// ---------- computeBackoff ----------

func TestComputeBackoff_ExponentialCurve(t *testing.T) {
	base := 30 * time.Second
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 30 * time.Second},
		{2, 1 * time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 8 * time.Minute},
		{6, 16 * time.Minute},
		{7, 32 * time.Minute},
		{8, time.Hour}, // cap kicks in
		{20, time.Hour},
	}
	for _, c := range cases {
		got := computeBackoff(c.attempt, base)
		if got != c.want {
			t.Errorf("attempt=%d: got %s, want %s", c.attempt, got, c.want)
		}
	}
}

func TestComputeBackoff_HandlesNonPositive(t *testing.T) {
	if got := computeBackoff(0, 10*time.Second); got != 10*time.Second {
		t.Errorf("attempt=0: got %s, want 10s (treated as 1)", got)
	}
	if got := computeBackoff(-3, 10*time.Second); got != 10*time.Second {
		t.Errorf("attempt=-3: got %s, want 10s (treated as 1)", got)
	}
}

// ---------- processOne behaviour ----------

func mustDispatcher(t *testing.T, ob persistence.Outbox, hs Handlers) *Dispatcher {
	t.Helper()
	d, err := NewDispatcher(validCfg(), DispatcherDeps{
		Pool:     &pgxpool.Pool{},
		Outbox:   ob,
		Logger:   discardLogger(),
		Handlers: hs,
		Clock:    frozenClock(time.Unix(1_700_000_000, 0).UTC(), 0),
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	return d
}

func TestProcessOne_Success_MarksDispatched(t *testing.T) {
	evt := mkEvent("widget.created", 0)
	ob := newFakeDispatchOutbox(evt)
	called := false
	d := mustDispatcher(t, ob, Handlers{
		"widget.created": HandlerFunc(func(_ context.Context, e persistence.OutboxEvent) error {
			called = true
			if e.ID != evt.ID {
				t.Errorf("handler got wrong event id")
			}
			return nil
		}),
	})

	d.processOne(context.Background(), evt)

	if !called {
		t.Fatal("handler was not invoked")
	}
	if got := ob.callCount("MarkDispatched"); got != 1 {
		t.Errorf("MarkDispatched: got %d want 1", got)
	}
	if got := ob.callCount("MarkFailed"); got != 0 {
		t.Errorf("MarkFailed: got %d want 0", got)
	}
	if got := ob.callCount("MarkPermanentlyFailed"); got != 0 {
		t.Errorf("MarkPermanentlyFailed: got %d want 0", got)
	}
	snap, ok := ob.snapshot(evt.ID)
	if !ok || snap.Status != persistence.OutboxDispatched {
		t.Errorf("status: got %q want dispatched", snap.Status)
	}
}

func TestProcessOne_HandlerError_SchedulesRetry(t *testing.T) {
	evt := mkEvent("widget.created", 1) // attempts=1, next will be 2
	ob := newFakeDispatchOutbox(evt)
	d := mustDispatcher(t, ob, Handlers{
		"widget.created": HandlerFunc(func(context.Context, persistence.OutboxEvent) error {
			return errors.New("flaky api")
		}),
	})

	d.processOne(context.Background(), evt)

	if got := ob.callCount("MarkFailed"); got != 1 {
		t.Errorf("MarkFailed: got %d want 1", got)
	}
	if got := ob.callCount("MarkPermanentlyFailed"); got != 0 {
		t.Errorf("MarkPermanentlyFailed: got %d want 0", got)
	}
	snap, _ := ob.snapshot(evt.ID)
	if snap.Status != persistence.OutboxPending {
		t.Errorf("status: got %q want pending (will retry)", snap.Status)
	}
	if snap.Attempts != 2 {
		t.Errorf("attempts: got %d want 2 (was 1, +1)", snap.Attempts)
	}
	if snap.LastError != "flaky api" {
		t.Errorf("last_error: got %q want %q", snap.LastError, "flaky api")
	}
	// Backoff for attempt=2 = base * 2 = 20s.
	gotBackoff := snap.NextAttemptAt.Sub(time.Unix(1_700_000_000, 0).UTC())
	wantBackoff := 20 * time.Second
	if gotBackoff != wantBackoff {
		t.Errorf("backoff: got %s want %s", gotBackoff, wantBackoff)
	}
}

func TestProcessOne_ExhaustedAttempts_MarksPermanentlyFailed(t *testing.T) {
	// validCfg.MaxAttempts=3; event with attempts=2 ??? next=3 ??? trigger.
	evt := mkEvent("widget.created", 2)
	ob := newFakeDispatchOutbox(evt)
	d := mustDispatcher(t, ob, Handlers{
		"widget.created": HandlerFunc(func(context.Context, persistence.OutboxEvent) error {
			return errors.New("still broken")
		}),
	})

	d.processOne(context.Background(), evt)

	if got := ob.callCount("MarkPermanentlyFailed"); got != 1 {
		t.Errorf("MarkPermanentlyFailed: got %d want 1", got)
	}
	if got := ob.callCount("MarkFailed"); got != 0 {
		t.Errorf("MarkFailed: got %d want 0 (skipped, gave up)", got)
	}
	snap, _ := ob.snapshot(evt.ID)
	if snap.Status != persistence.OutboxFailed {
		t.Errorf("status: got %q want failed", snap.Status)
	}
	if snap.LastError != "still broken" {
		t.Errorf("last_error: got %q want %q", snap.LastError, "still broken")
	}
}

func TestProcessOne_UnknownEventType_MarksPermanentlyFailed(t *testing.T) {
	evt := mkEvent("unknown.event", 0)
	ob := newFakeDispatchOutbox(evt)
	d := mustDispatcher(t, ob, Handlers{
		"widget.created": HandlerFunc(func(context.Context, persistence.OutboxEvent) error { return nil }),
	})

	d.processOne(context.Background(), evt)

	if got := ob.callCount("MarkPermanentlyFailed"); got != 1 {
		t.Errorf("MarkPermanentlyFailed: got %d want 1", got)
	}
	snap, _ := ob.snapshot(evt.ID)
	if snap.Status != persistence.OutboxFailed {
		t.Errorf("status: got %q want failed", snap.Status)
	}
	if want := "no handler"; !contains(snap.LastError, want) {
		t.Errorf("last_error: got %q want substring %q", snap.LastError, want)
	}
}

func TestProcessOne_HandlerTimeoutCancelsCtx(t *testing.T) {
	evt := mkEvent("slow.event", 0)
	ob := newFakeDispatchOutbox(evt)
	cfg := validCfg()
	cfg.HandlerTimeout = 50 * time.Millisecond
	d, err := NewDispatcher(cfg, DispatcherDeps{
		Pool:   &pgxpool.Pool{},
		Outbox: ob,
		Logger: discardLogger(),
		Handlers: Handlers{
			"slow.event": HandlerFunc(func(ctx context.Context, _ persistence.OutboxEvent) error {
				select {
				case <-time.After(500 * time.Millisecond):
					return errors.New("slept past timeout")
				case <-ctx.Done():
					return ctx.Err()
				}
			}),
		},
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	d.processOne(context.Background(), evt)

	if got := ob.callCount("MarkFailed"); got != 1 {
		t.Errorf("MarkFailed: got %d want 1 (timeout = retryable failure)", got)
	}
	snap, _ := ob.snapshot(evt.ID)
	if !contains(snap.LastError, "context deadline exceeded") {
		t.Errorf("last_error: got %q want substring 'context deadline exceeded'", snap.LastError)
	}
}

// ---------- drainOnce / drainAll ----------

func TestDrainOnce_ProcessesAllPending(t *testing.T) {
	evts := []persistence.OutboxEvent{
		mkEvent("a", 0),
		mkEvent("a", 0),
		mkEvent("a", 0),
	}
	ob := newFakeDispatchOutbox(evts...)
	d := mustDispatcher(t, ob, Handlers{
		"a": HandlerFunc(func(context.Context, persistence.OutboxEvent) error { return nil }),
	})

	var wg sync.WaitGroup
	n := d.drainOnce(context.Background(), &wg)

	if n != 3 {
		t.Errorf("processed: got %d want 3", n)
	}
	if got := ob.callCount("MarkDispatched"); got != 3 {
		t.Errorf("MarkDispatched: got %d want 3", got)
	}
}

func TestDrainAll_LoopsWhenBatchIsFull(t *testing.T) {
	// BatchSize=8 in validCfg. Create 10 events ??? first drain returns 8,
	// drainAll loops, second drain returns 2.
	evts := make([]persistence.OutboxEvent, 10)
	for i := range evts {
		evts[i] = mkEvent("a", 0)
	}
	ob := newFakeDispatchOutbox(evts...)
	d := mustDispatcher(t, ob, Handlers{
		"a": HandlerFunc(func(context.Context, persistence.OutboxEvent) error { return nil }),
	})

	var wg sync.WaitGroup
	d.drainAll(context.Background(), &wg)

	if got := ob.callCount("MarkDispatched"); got != 10 {
		t.Errorf("MarkDispatched: got %d want 10", got)
	}
	if got := ob.callCount("ListPending"); got < 2 {
		t.Errorf("ListPending: got %d want >= 2 (loop continuation)", got)
	}
}

func TestDrainOnce_RespectsCtxCancel(t *testing.T) {
	evts := []persistence.OutboxEvent{mkEvent("a", 0), mkEvent("a", 0)}
	ob := newFakeDispatchOutbox(evts...)

	ctx, cancel := context.WithCancel(context.Background())
	d := mustDispatcher(t, ob, Handlers{
		"a": HandlerFunc(func(_ context.Context, _ persistence.OutboxEvent) error {
			cancel() // cancel mid-batch
			return nil
		}),
	})

	var wg sync.WaitGroup
	d.drainOnce(ctx, &wg)

	// After cancel, the second event should NOT have been processed.
	// MarkDispatched should be 1 (the first event), not 2.
	if got := ob.callCount("MarkDispatched"); got != 1 {
		t.Errorf("MarkDispatched: got %d want 1 (cancel mid-batch should stop)", got)
	}
}

// ---------- helpers ----------

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
outer:
	for i := 0; i+len(sub) <= len(s); i++ {
		for j := 0; j < len(sub); j++ {
			if s[i+j] != sub[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
