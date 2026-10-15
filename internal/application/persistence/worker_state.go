package persistence

import (
	"context"
	"encoding/json"
)

// WorkerState is the port for the singleton key/value table used by
// the M10 worker subsystems to remember "where I was" across restarts.
// It deliberately exposes only Get and Set — concurrency invariants
// live at the layer that opens a UnitOfWork transaction wrapping the
// read, the side-effect (e.g. outbox.Enqueue), and the write.
//
// Conventions:
//
//   - The key namespace is owned by the calling subsystem. Use a
//     "<subsystem>:<scope>" prefix to avoid collisions. Examples:
//
//     "ticker:sprint:<sprint_uuid>"      // last seen sprint snapshot
//     "ticker:last_run_at"               // wall-clock bookmark
//     "webhook_replay:cursor"            // future use
//
//   - Value bytes must be valid JSON. The adapter stores them as
//     JSONB (introspectable from psql, queryable via jsonb_path_ops).
//     Subsystems define their own shape; the port stays generic.
//
//   - Get on a missing key returns persistence.ErrNotFound (mirrors
//     every other Find* method in this package).
//
//   - Set is an upsert: insert when the row is missing, update value
//     and updated_at when present.
type WorkerState interface {
	// Get returns the raw JSON value for key. Returns ErrNotFound
	// when no row exists.
	Get(ctx context.Context, key string) (json.RawMessage, error)

	// Set inserts or updates the value for key. The adapter stamps
	// updated_at = now() on every call.
	Set(ctx context.Context, key string, value json.RawMessage) error
}
