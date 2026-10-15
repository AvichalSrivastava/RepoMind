// Package persistence defines the repository ports (interfaces) that
// the application layer uses to read and write aggregates. The actual
// implementations live in internal/infrastructure/postgres.
//
// Design notes:
//
//   - Methods take context.Context first and return wrapped errors;
//     callers must use errors.Is(err, persistence.ErrNotFound) etc.
//   - Sentinels are defined here, NOT re-exported from the postgres
//     adapter, so the application layer never imports the adapter.
//   - All read-modify-write workflows go through UnitOfWork.Within so
//     domain mutations and outbox enqueues commit atomically.
package persistence

import "errors"

// Sentinel errors. Adapters MUST translate driver-specific errors
// (pgx.ErrNoRows, unique violation 23505, etc.) into these so the
// application layer can switch on them without importing pgx.
var (
	// ErrNotFound — no aggregate matched the query.
	ErrNotFound = errors.New("persistence: not found")

	// ErrConflict — unique-constraint or version-check violation.
	ErrConflict = errors.New("persistence: conflict")

	// ErrSerialization — transaction aborted due to serialization
	// failure (PostgreSQL 40001). Callers may retry the entire
	// UnitOfWork.Within block.
	ErrSerialization = errors.New("persistence: serialization failure")
)
