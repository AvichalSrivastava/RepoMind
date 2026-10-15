// Package orchestration provides the M9/M10 workflow plumbing that
// sits between the HTTP / webhook ingress and the durable per-event
// handlers.
//
// Dispatcher (this file) is the heart of M10:
//
//   - Holds a dedicated Postgres connection LISTENing on the channel
//     persistence.NotifyChannel (sub-second wake-ups on new outbox
//     rows).
//   - Falls back to a periodic poll (cfg.PollInterval) so missed
//     NOTIFYs during reconnect, or events whose next_attempt_at has
//     just become due, still get picked up.
//   - On each wake, drains the outbox in batches via
//     persistence.Outbox.ListPending; routes each event to its
//     registered Handler keyed on event_type.
//   - On handler success: MarkDispatched. On retryable failure:
//     MarkFailed with exponential backoff (BaseBackoff * 2^(n-1),
//     capped at 1h). On Nth failure (cfg.MaxAttempts): MarkPermanentlyFailed.
//   - On ctx cancel: stops the drain loop, waits up to
//     cfg.ShutdownGrace for in-flight handlers, returns.
//
// Concurrency model (V1): one drain goroutine, serial handler
// invocation. This keeps the dispatcher trivially observable and
// avoids cross-handler ordering surprises in tests. A worker pool
// can be added later behind the same Handler interface without
// changing callers.
//
// Multi-instance safety (V1): SINGLE-INSTANCE. If two RepoMind
// processes share the same outbox table, both will pick up the same
// pending rows and double-dispatch. Documented as future work in
// docs/ARCHITECTURE.md §6. The fix is to wrap drain in a transaction
// using SELECT FOR UPDATE SKIP LOCKED.
package orchestration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/repomind/repomind/internal/application/persistence"
)

// Handler processes one outbox event. Implementations MUST honour
// ctx.Done() (the dispatcher cancels it on shutdown and on
// HandlerTimeout). Returning nil means "success, mark dispatched".
// Returning a non-nil error schedules a retry until MaxAttempts is
// exhausted.
type Handler interface {
	Handle(ctx context.Context, evt persistence.OutboxEvent) error
}

// HandlerFunc lets a plain function satisfy Handler.
type HandlerFunc func(ctx context.Context, evt persistence.OutboxEvent) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, evt persistence.OutboxEvent) error {
	return f(ctx, evt)
}

// Handlers is the event_type → Handler registry.
type Handlers map[string]Handler

// DispatcherConfig is what the dispatcher needs at construction time.
// All durations must be > 0; BatchSize and MaxAttempts > 0.
type DispatcherConfig struct {
	PollInterval   time.Duration
	BatchSize      int
	MaxAttempts    int
	BaseBackoff    time.Duration
	HandlerTimeout time.Duration
	ShutdownGrace  time.Duration
}

// DispatcherDeps are the runtime dependencies.
type DispatcherDeps struct {
	Pool     *pgxpool.Pool      // for the LISTEN-holding connection
	Outbox   persistence.Outbox // for ListPending / MarkDispatched / MarkFailed
	Handlers Handlers           // event_type → handler
	Logger   *slog.Logger       // structured logging
	Clock    func() time.Time   // for backoff math; nil → time.Now
}

// Dispatcher is the long-running outbox consumer.
type Dispatcher struct {
	cfg      DispatcherConfig
	pool     *pgxpool.Pool
	outbox   persistence.Outbox
	handlers Handlers
	logger   *slog.Logger
	now      func() time.Time
}

// NewDispatcher validates cfg + deps and returns a ready-to-Run
// dispatcher. nil dependencies and zero-valued config fields are
// rejected at construction (fail-fast) so the composition root
// surfaces problems before any Outbox row is at risk.
func NewDispatcher(cfg DispatcherConfig, deps DispatcherDeps) (*Dispatcher, error) {
	switch {
	case deps.Pool == nil:
		return nil, errors.New("orchestration: dispatcher: Pool is required")
	case deps.Outbox == nil:
		return nil, errors.New("orchestration: dispatcher: Outbox is required")
	case deps.Logger == nil:
		return nil, errors.New("orchestration: dispatcher: Logger is required")
	case len(deps.Handlers) == 0:
		return nil, errors.New("orchestration: dispatcher: at least one Handler must be registered")
	case cfg.PollInterval <= 0:
		return nil, errors.New("orchestration: dispatcher: PollInterval must be > 0")
	case cfg.BatchSize <= 0:
		return nil, errors.New("orchestration: dispatcher: BatchSize must be > 0")
	case cfg.MaxAttempts <= 0:
		return nil, errors.New("orchestration: dispatcher: MaxAttempts must be > 0")
	case cfg.BaseBackoff <= 0:
		return nil, errors.New("orchestration: dispatcher: BaseBackoff must be > 0")
	case cfg.HandlerTimeout <= 0:
		return nil, errors.New("orchestration: dispatcher: HandlerTimeout must be > 0")
	case cfg.ShutdownGrace <= 0:
		return nil, errors.New("orchestration: dispatcher: ShutdownGrace must be > 0")
	}
	now := deps.Clock
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	// Defensive: copy the handler map so registrants can't mutate
	// the dispatcher's view after construction.
	hs := make(Handlers, len(deps.Handlers))
	for k, v := range deps.Handlers {
		if v == nil {
			return nil, fmt.Errorf("orchestration: dispatcher: handler for %q is nil", k)
		}
		hs[k] = v
	}
	return &Dispatcher{
		cfg:      cfg,
		pool:     deps.Pool,
		outbox:   deps.Outbox,
		handlers: hs,
		logger:   deps.Logger,
		now:      now,
	}, nil
}

// RegisteredEventTypes returns the sorted set of event_types this
// dispatcher will handle. Useful for the composition root startup
// log so operators can confirm the wiring.
func (d *Dispatcher) RegisteredEventTypes() []string {
	out := make([]string, 0, len(d.handlers))
	for k := range d.handlers {
		out = append(out, k)
	}
	// Cheap insertion sort; len(handlers) is tiny.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Run blocks until ctx is cancelled, dispatching outbox events as
// they arrive. The first drain happens immediately (covers anything
// already pending when the worker started). Subsequent drains are
// triggered by NOTIFYs or PollInterval, whichever fires first.
//
// On ctx cancel, Run waits up to ShutdownGrace for in-flight handlers
// to finish before returning. A returned error indicates the
// dispatcher itself broke (e.g. Postgres conn lost permanently);
// per-event errors are absorbed into MarkFailed/MarkPermanentlyFailed
// and never surface here.
func (d *Dispatcher) Run(ctx context.Context) error {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("orchestration: dispatcher: acquire LISTEN conn: %w", err)
	}
	defer conn.Release()

	// Subscribe BEFORE the first drain so that NOTIFYs fired between
	// drain start and LISTEN registration aren't lost. (Strictly
	// speaking pg_notify after LISTEN is what matters, and any pre-
	// LISTEN notifies are still represented by a pending row, which
	// the immediate drain catches. Belt and braces.)
	if _, err := conn.Exec(ctx, "LISTEN "+persistence.NotifyChannel); err != nil {
		return fmt.Errorf("orchestration: dispatcher: LISTEN: %w", err)
	}
	d.logger.Info("dispatcher: started",
		"channel", persistence.NotifyChannel,
		"poll_interval", d.cfg.PollInterval,
		"batch_size", d.cfg.BatchSize,
		"max_attempts", d.cfg.MaxAttempts,
		"handlers", d.RegisteredEventTypes(),
	)

	var inflight sync.WaitGroup
	defer d.gracefulWait(&inflight)

	// Initial drain — catches rows already pending at startup.
	d.drainAll(ctx, &inflight)

	for {
		if ctx.Err() != nil {
			d.logger.Info("dispatcher: ctx cancelled, stopping")
			return nil
		}

		// Wait for either a NOTIFY or PollInterval. The timeout
		// serves double duty as the poll trigger.
		waitCtx, cancel := context.WithTimeout(ctx, d.cfg.PollInterval)
		_, notifyErr := conn.Conn().WaitForNotification(waitCtx)
		cancel()

		switch {
		case errors.Is(notifyErr, context.Canceled):
			// Parent ctx cancelled; loop top will exit.
			continue
		case errors.Is(notifyErr, context.DeadlineExceeded):
			// Poll timer fired; this is the safety-net drain.
			d.drainAll(ctx, &inflight)
		case notifyErr != nil:
			// Real error on the conn (e.g. Postgres restarted).
			// Log + sleep briefly to avoid hot-looping on a broken
			// conn; the next iteration's WaitForNotification or
			// the deferred Release will surface the situation.
			d.logger.Error("dispatcher: WaitForNotification error",
				"err", notifyErr)
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
			}
		default:
			// Got a NOTIFY -- drain.
			d.drainAll(ctx, &inflight)
		}
	}
}

// drainAll loops drainOnce until a non-full batch arrives, ensuring
// a backlog larger than BatchSize is fully consumed in a single wake.
func (d *Dispatcher) drainAll(ctx context.Context, inflight *sync.WaitGroup) {
	for {
		if ctx.Err() != nil {
			return
		}
		n := d.drainOnce(ctx, inflight)
		if n < d.cfg.BatchSize {
			return
		}
	}
}

// drainOnce fetches up to BatchSize pending events and processes
// each serially. Returns the number of events processed (0 means
// nothing was pending; caller should stop looping).
func (d *Dispatcher) drainOnce(ctx context.Context, inflight *sync.WaitGroup) int {
	evts, err := d.outbox.ListPending(ctx, d.cfg.BatchSize)
	if err != nil {
		// ListPending failures are transient (most likely Postgres
		// hiccup). Log, return 0 so the loop pauses; next poll/notify
		// will try again.
		d.logger.Error("dispatcher: ListPending failed", "err", err)
		return 0
	}
	for _, evt := range evts {
		if ctx.Err() != nil {
			return len(evts)
		}
		inflight.Add(1)
		d.processOne(ctx, evt)
		inflight.Done()
	}
	return len(evts)
}

// processOne routes a single event to its handler, applies the
// success/retry/give-up policy, and writes the outcome back to the
// outbox row. Never returns an error: outcomes are recorded
// in-band; only the dispatcher loop's own infra problems propagate.
func (d *Dispatcher) processOne(ctx context.Context, evt persistence.OutboxEvent) {
	logger := d.logger.With(
		"event_id", evt.ID.String(),
		"event_type", evt.EventType,
		"attempts", evt.Attempts,
	)

	h, ok := d.handlers[evt.EventType]
	if !ok {
		// No handler registered. This is an operator bug (a new
		// producer was deployed without the matching consumer).
		// Fail the row permanently so the dispatcher doesn't loop
		// on it forever; the operator sees it in the
		// status='failed' bucket with a clear message.
		msg := "no handler registered for event_type=" + evt.EventType
		logger.Error("dispatcher: " + msg)
		if err := d.outbox.MarkPermanentlyFailed(ctx, evt.ID, msg); err != nil {
			logger.Error("dispatcher: MarkPermanentlyFailed failed", "err", err)
		}
		return
	}

	// Per-handler timeout. Independent context so the handler
	// can't keep work alive past the budget.
	handlerCtx, cancel := context.WithTimeout(ctx, d.cfg.HandlerTimeout)
	defer cancel()

	start := d.now()
	handlerErr := h.Handle(handlerCtx, evt)
	elapsed := d.now().Sub(start)

	if handlerErr == nil {
		if err := d.outbox.MarkDispatched(ctx, evt.ID, d.now()); err != nil {
			logger.Error("dispatcher: MarkDispatched failed",
				"err", err, "duration", elapsed)
			return
		}
		logger.Info("dispatcher: event dispatched", "duration", elapsed)
		return
	}

	// Handler failed. Decide between retry and give-up.
	nextAttempt := evt.Attempts + 1
	if nextAttempt >= d.cfg.MaxAttempts {
		logger.Error("dispatcher: event exhausted retries, marking failed",
			"err", handlerErr, "duration", elapsed)
		if err := d.outbox.MarkPermanentlyFailed(ctx, evt.ID, handlerErr.Error()); err != nil {
			logger.Error("dispatcher: MarkPermanentlyFailed failed", "err", err)
		}
		return
	}

	backoff := computeBackoff(nextAttempt, d.cfg.BaseBackoff)
	nextAt := d.now().Add(backoff)
	logger.Warn("dispatcher: event failed, will retry",
		"err", handlerErr,
		"duration", elapsed,
		"backoff", backoff,
		"next_attempt_at", nextAt,
	)
	if err := d.outbox.MarkFailed(ctx, evt.ID, handlerErr.Error(), nextAt); err != nil {
		logger.Error("dispatcher: MarkFailed failed", "err", err)
	}
}

// computeBackoff returns BaseBackoff * 2^(attempt-1), capped at 1 hour.
// attempt is 1-based: the first retry uses BaseBackoff exactly.
//
//	attempt=1 → 30s   (base)
//	attempt=2 → 1m
//	attempt=3 → 2m
//	attempt=4 → 4m
//	attempt=5 → 8m
//	... capped at 1h.
func computeBackoff(attempt int, base time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Cap the shift to avoid overflow. 2^30 * base would be way past
	// any reasonable cap anyway; we apply the 1h cap immediately.
	if attempt > 30 {
		return time.Hour
	}
	d := base << (attempt - 1)
	if d > time.Hour || d < 0 {
		return time.Hour
	}
	return d
}

// gracefulWait waits for in-flight handlers to finish, up to
// ShutdownGrace. Past that, returns and lets the deferred conn
// release tear down the LISTEN. Any in-flight handler that didn't
// finish in time will have its handlerCtx cancelled by the parent
// ctx already; the outbox row stays at status='pending' so the next
// process start retries it.
func (d *Dispatcher) gracefulWait(inflight *sync.WaitGroup) {
	done := make(chan struct{})
	go func() {
		inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		d.logger.Info("dispatcher: graceful shutdown complete")
	case <-time.After(d.cfg.ShutdownGrace):
		d.logger.Warn("dispatcher: shutdown grace expired with in-flight work")
	}
}
