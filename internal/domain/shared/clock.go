package shared

import "time"

// Clock abstracts time.Now so domain code is testable. Aggregates never
// call time.Now() directly — they accept a Clock or are given a
// timestamp by the application layer.
type Clock interface {
	Now() time.Time
}

// SystemClock returns wall-clock time.
type SystemClock struct{}

// Now returns time.Now().UTC() — domain timestamps are always UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock is a deterministic Clock for tests.
type FixedClock struct {
	T time.Time
}

// Now returns the fixed instant in UTC.
func (f FixedClock) Now() time.Time { return f.T.UTC() }
