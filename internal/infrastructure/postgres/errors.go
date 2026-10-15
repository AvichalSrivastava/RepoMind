package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/repomind/repomind/internal/application/persistence"
)

// classify translates a driver-level error into one of the application
// sentinels (persistence.ErrNotFound, ErrConflict, ErrSerialization)
// when applicable, otherwise wraps it for context.
//
// Callers wrap with operation-specific context BEFORE calling classify,
// e.g.:  return classify("save pull_request", err)
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	// Cancellation / deadline pass through verbatim so callers can
	// detect ctx.Err() with errors.Is.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("postgres %s: %w", op, persistence.ErrNotFound)
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgerrcode.UniqueViolation,
			pgerrcode.ExclusionViolation,
			pgerrcode.ForeignKeyViolation:
			return fmt.Errorf("postgres %s: %w: %s", op, persistence.ErrConflict, pgErr.Message)
		case pgerrcode.SerializationFailure,
			pgerrcode.DeadlockDetected:
			return fmt.Errorf("postgres %s: %w: %s", op, persistence.ErrSerialization, pgErr.Message)
		case pgerrcode.CheckViolation,
			pgerrcode.NotNullViolation:
			// Invariant breach — surface as conflict so callers see
			// it as "the data violates a constraint" rather than a
			// generic error.
			return fmt.Errorf("postgres %s: %w: %s", op, persistence.ErrConflict, pgErr.Message)
		}
	}

	return fmt.Errorf("postgres %s: %w", op, err)
}
