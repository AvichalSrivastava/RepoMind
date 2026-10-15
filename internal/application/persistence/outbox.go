package persistence

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// OutboxEvent is a single side-effect that must be dispatched after
// the parent DB transaction commits. It is written in the same tx as
// the aggregate it relates to, then later picked up by a worker that
// publishes to the target system (Confluence, webhook receiver, …).
//
// The aggregate_type / aggregate_id pair is informational — useful for
// dashboards and debugging. The event_type drives dispatch routing.
//
// DedupKey (added in M9) is an optional opaque key supplied by the
// caller (X-GitHub-Delivery header for webhooks, Idempotency-Key
// header for operator calls). When set, EnqueueWithDedup turns retries
// into no-ops at the enqueue layer. Empty string = no dedup.
type OutboxEvent struct {
	ID            uuid.UUID
	AggregateType string
	AggregateID   uuid.UUID
	EventType     string
	Payload       json.RawMessage
	DedupKey      string
	Status        OutboxStatus
	Attempts      int
	LastError     string
	NextAttemptAt time.Time
	DispatchedAt  *time.Time
	CreatedAt     time.Time
}

// OutboxStatus mirrors the CHECK constraint on outbox.status.
type OutboxStatus string

const (
	OutboxPending    OutboxStatus = "pending"
	OutboxDispatched OutboxStatus = "dispatched"
	OutboxFailed     OutboxStatus = "failed"
)

// NotifyChannel is the Postgres LISTEN/NOTIFY channel name the outbox
// adapter publishes to whenever a new row is inserted (M9). M10's
// dispatcher LISTENs on this channel for sub-second wake-ups; a
// periodic poll covers any NOTIFY missed during reconnect.
const NotifyChannel = "repomind_outbox"

// EnqueueResult is what EnqueueWithDedup returns. When DedupHit is
// true, the row already existed (a prior insert with the same
// DedupKey) and ID points at that prior row — no new row was created
// and no NOTIFY was fired. Callers (webhook handlers) should respond
// with the cached id so retries observe idempotent behaviour.
type EnqueueResult struct {
	ID       uuid.UUID
	DedupHit bool
}

// Outbox is the port for the transactional outbox. Producers call
// Enqueue from inside a UnitOfWork.Within block; the worker calls
// ListPending / MarkDispatched / MarkFailed.
type Outbox interface {
	// Enqueue inserts a new pending outbox row. ID, Status,
	// CreatedAt and NextAttemptAt are filled in by the adapter
	// when zero; everything else is caller-supplied. Always emits
	// a NOTIFY on NotifyChannel after insert.
	Enqueue(ctx context.Context, e OutboxEvent) error

	// EnqueueWithDedup is Enqueue with at-most-once semantics keyed
	// on e.DedupKey (which MUST be non-empty). If a prior row with
	// the same DedupKey exists, no new row is inserted and the
	// result carries DedupHit=true with the prior row's ID. NOTIFY
	// is fired only on real inserts.
	EnqueueWithDedup(ctx context.Context, e OutboxEvent) (EnqueueResult, error)

	// ListPending returns up to `limit` pending events whose
	// next_attempt_at <= now(), ordered by next_attempt_at. Used
	// only by the dispatch worker (Module 10).
	ListPending(ctx context.Context, limit int) ([]OutboxEvent, error)

	// MarkDispatched flips status to 'dispatched' and stamps
	// dispatched_at. Idempotent if the event is already dispatched.
	MarkDispatched(ctx context.Context, id uuid.UUID, at time.Time) error

	// MarkFailed records a failed attempt and schedules a retry.
	// The caller (worker) supplies the next_attempt_at after
	// applying its own backoff policy. The adapter increments
	// attempts and stores last_error; status stays 'pending' so
	// the row is eligible to be picked up again at next_attempt_at.
	// If the caller wants to give up permanently, it can write
	// status='failed' separately via a future helper.
	MarkFailed(ctx context.Context, id uuid.UUID, lastErr string, nextAttemptAt time.Time) error

	// MarkPermanentlyFailed flips status to 'failed' so the row is
	// no longer eligible to be retried by the dispatcher's drain
	// pass. Used when:
	//   - max retry attempts exhausted
	//   - no handler is registered for the event_type (operator bug)
	//   - the handler returned a non-retryable error category
	//
	// Increments attempts and stamps last_error one final time so
	// the dashboard shows the terminal failure cause. Idempotent —
	// calling it on an already-failed row is a no-op (still returns
	// nil).
	MarkPermanentlyFailed(ctx context.Context, id uuid.UUID, lastErr string) error
}
