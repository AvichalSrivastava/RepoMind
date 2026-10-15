package shared

import "errors"

// Sentinel errors used across the domain. Application and infrastructure
// layers check with errors.Is to produce appropriate transport responses
// (e.g. 404 for ErrNotFound).
var (
	// ErrNotFound is returned by repositories when no aggregate matches.
	ErrNotFound = errors.New("not found")

	// ErrAlreadyExists is returned when a uniqueness constraint is hit.
	ErrAlreadyExists = errors.New("already exists")

	// ErrConflict signals an optimistic-concurrency or invariant violation.
	ErrConflict = errors.New("conflict")

	// ErrInvariant signals an attempted operation that would break an
	// aggregate invariant (e.g. merging an already-merged PR).
	ErrInvariant = errors.New("domain invariant violated")

	// ErrInvalidArgument signals bad input at the domain boundary.
	ErrInvalidArgument = errors.New("invalid argument")
)

// ValidationError is a typed error that carries the offending field
// alongside the cause. Wraps ErrInvalidArgument so callers can use
// errors.Is(err, ErrInvalidArgument).
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Message
}

// Is satisfies errors.Is for ErrInvalidArgument.
func (e *ValidationError) Is(target error) bool {
	return target == ErrInvalidArgument
}

// NewValidationError builds a ValidationError.
func NewValidationError(field, message string) *ValidationError {
	return &ValidationError{Field: field, Message: message}
}
