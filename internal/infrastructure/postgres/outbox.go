package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/repomind/repomind/internal/application/persistence"
)

type outboxRepo struct{ q dbtx }

var _ persistence.Outbox = (*outboxRepo)(nil)

const outboxSelectCols = `
	id, aggregate_type, aggregate_id, event_type, payload,
	status, attempts, last_error, next_attempt_at, dispatched_at, created_at,
	dedup_key
`

// stampDefaults fills zero fields on a fresh OutboxEvent.
func stampDefaults(e *persistence.OutboxEvent) {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.Status == "" {
		e.Status = persistence.OutboxPending
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.NextAttemptAt.IsZero() {
		e.NextAttemptAt = e.CreatedAt
	}
}

func (r *outboxRepo) Enqueue(ctx context.Context, e persistence.OutboxEvent) error {
	stampDefaults(&e)

	// Plain INSERT. The wake-up NOTIFY is fired by the
	// outbox_notify_trg trigger (migration 000004), which calls
	// pg_notify on COMMIT regardless of whether we're inside an
	// explicit transaction or running pool-direct. Keeping the
	// SQL boring here means any future inserter (psql, M10 worker,
	// scripts) wakes listeners automatically. DedupKey is stored
	// as NULL when empty so the UNIQUE index doesn't block plain
	// callers that opt out of dedup.
	const q = `
INSERT INTO outbox (
    id, aggregate_type, aggregate_id, event_type, payload,
    status, attempts, last_error, next_attempt_at, dispatched_at,
    created_at, dedup_key
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''))`

	_, err := r.q.Exec(ctx, q,
		e.ID,
		e.AggregateType,
		e.AggregateID,
		e.EventType,
		[]byte(e.Payload),
		string(e.Status),
		e.Attempts,
		e.LastError,
		e.NextAttemptAt,
		e.DispatchedAt,
		e.CreatedAt,
		e.DedupKey,
	)
	return classify("enqueue outbox", err)
}

func (r *outboxRepo) EnqueueWithDedup(
	ctx context.Context,
	e persistence.OutboxEvent,
) (persistence.EnqueueResult, error) {
	if e.DedupKey == "" {
		return persistence.EnqueueResult{},
			errors.New("postgres: EnqueueWithDedup requires non-empty DedupKey")
	}
	stampDefaults(&e)

	// Strategy: try INSERT first; if dedup hit, the UNIQUE on
	// dedup_key triggers a 23505 (unique_violation), which we catch
	// and resolve via a SELECT for the existing id. Two roundtrips
	// in the rare collision case; one in the common case. The
	// outbox_notify_trg trigger (migration 000004) fires NOTIFY
	// only on the successful-insert path -- 23505 rolls the INSERT
	// back before the trigger commits, so duplicates correctly do
	// NOT wake listeners.
	const ins = `
INSERT INTO outbox (
    id, aggregate_type, aggregate_id, event_type, payload,
    status, attempts, last_error, next_attempt_at, dispatched_at,
    created_at, dedup_key
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`

	_, err := r.q.Exec(ctx, ins,
		e.ID,
		e.AggregateType,
		e.AggregateID,
		e.EventType,
		[]byte(e.Payload),
		string(e.Status),
		e.Attempts,
		e.LastError,
		e.NextAttemptAt,
		e.DispatchedAt,
		e.CreatedAt,
		e.DedupKey,
	)

	if err == nil {
		// Stamped e.ID was used in the INSERT; safe to return.
		return persistence.EnqueueResult{ID: e.ID, DedupHit: false}, nil
	}

	// 23505 = unique_violation on outbox_dedup_key_uidx → dedup hit.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
		const sel = `SELECT id FROM outbox WHERE dedup_key = $1 LIMIT 1`
		var existingID uuid.UUID
		if selErr := r.q.QueryRow(ctx, sel, e.DedupKey).Scan(&existingID); selErr != nil {
			return persistence.EnqueueResult{},
				classify("enqueue dedup lookup", selErr)
		}
		return persistence.EnqueueResult{ID: existingID, DedupHit: true}, nil
	}

	return persistence.EnqueueResult{}, classify("enqueue outbox", err)
}

func (r *outboxRepo) ListPending(ctx context.Context, limit int) ([]persistence.OutboxEvent, error) {
	if limit <= 0 {
		limit = 32
	}
	const q = `
SELECT ` + outboxSelectCols + `
FROM outbox
WHERE status = 'pending' AND next_attempt_at <= now()
ORDER BY next_attempt_at
LIMIT $1`
	rows, err := r.q.Query(ctx, q, limit)
	if err != nil {
		return nil, classify("list pending outbox", err)
	}
	defer rows.Close()

	var out []persistence.OutboxEvent
	for rows.Next() {
		var (
			e             persistence.OutboxEvent
			statusStr     string
			payloadB      []byte
			dispatchedAt  *time.Time
			nextAttemptAt time.Time
			createdAt     time.Time
			dedupKey      *string
		)
		if err := rows.Scan(
			&e.ID, &e.AggregateType, &e.AggregateID, &e.EventType, &payloadB,
			&statusStr, &e.Attempts, &e.LastError, &nextAttemptAt, &dispatchedAt, &createdAt,
			&dedupKey,
		); err != nil {
			return nil, classify("scan outbox", err)
		}
		e.Status = persistence.OutboxStatus(statusStr)
		e.Payload = append([]byte(nil), payloadB...)
		e.NextAttemptAt = nextAttemptAt.UTC()
		e.DispatchedAt = ptrUTC(dispatchedAt)
		e.CreatedAt = createdAt.UTC()
		if dedupKey != nil {
			e.DedupKey = *dedupKey
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, classify("list pending outbox: rows", err)
	}
	return out, nil
}

func (r *outboxRepo) MarkDispatched(ctx context.Context, id uuid.UUID, at time.Time) error {
	const q = `
UPDATE outbox
SET status = 'dispatched', dispatched_at = $2
WHERE id = $1`
	tag, err := r.q.Exec(ctx, q, id, at)
	if err != nil {
		return classify("mark dispatched", err)
	}
	if tag.RowsAffected() == 0 {
		return classify("mark dispatched", pgx.ErrNoRows)
	}
	return nil
}

func (r *outboxRepo) MarkFailed(
	ctx context.Context,
	id uuid.UUID,
	lastErr string,
	nextAttemptAt time.Time,
) error {
	const q = `
UPDATE outbox
SET attempts = attempts + 1,
    last_error = $2,
    next_attempt_at = $3
WHERE id = $1`
	tag, err := r.q.Exec(ctx, q, id, lastErr, nextAttemptAt)
	if err != nil {
		return classify("mark failed", err)
	}
	if tag.RowsAffected() == 0 {
		return classify("mark failed", pgx.ErrNoRows)
	}
	return nil
}

// MarkPermanentlyFailed transitions the row to status='failed' so
// the dispatcher's drain pass (which filters on status='pending')
// stops considering it. We bump attempts + stamp last_error one
// final time for forensic value -- the dashboard / operator can see
// exactly which retry was the last one.
//
// Idempotent: rows already at status='failed' return 0 rows-affected,
// which we tolerate and return nil for. Calling this on a row that
// doesn't exist at all also returns nil (it's terminal either way --
// the dispatcher should treat "row gone" the same as "row failed").
func (r *outboxRepo) MarkPermanentlyFailed(
	ctx context.Context,
	id uuid.UUID,
	lastErr string,
) error {
	const q = `
UPDATE outbox
SET status     = 'failed',
    attempts   = attempts + 1,
    last_error = $2
WHERE id = $1 AND status <> 'failed'`
	_, err := r.q.Exec(ctx, q, id, lastErr)
	if err != nil {
		return classify("mark permanently failed", err)
	}
	return nil
}
