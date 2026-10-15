package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/repomind/repomind/internal/application/persistence"
)

type workerStateRepo struct{ q dbtx }

var _ persistence.WorkerState = (*workerStateRepo)(nil)

func (r *workerStateRepo) Get(ctx context.Context, key string) (json.RawMessage, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("postgres: worker_state key is required")
	}
	const q = `SELECT value FROM worker_state WHERE key = $1`
	var raw []byte
	err := r.q.QueryRow(ctx, q, key).Scan(&raw)
	if err != nil {
		// classify maps pgx.ErrNoRows -> persistence.ErrNotFound.
		return nil, classify("worker_state get", err)
	}
	// Defensive copy — pgx may reuse the underlying buffer.
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out, nil
}

func (r *workerStateRepo) Set(ctx context.Context, key string, value json.RawMessage) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("postgres: worker_state key is required")
	}
	// Empty value is allowed (defaults to '{}' via column default if
	// caller passes nil/empty). pgx maps nil []byte to NULL though,
	// which violates NOT NULL — coerce to an empty JSON object so the
	// caller doesn't have to remember.
	if len(value) == 0 {
		value = json.RawMessage(`{}`)
	}
	const q = `
INSERT INTO worker_state (key, value, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (key) DO UPDATE
SET value      = EXCLUDED.value,
    updated_at = EXCLUDED.updated_at`
	if _, err := r.q.Exec(ctx, q, key, []byte(value)); err != nil {
		return classify("worker_state set", err)
	}
	return nil
}
