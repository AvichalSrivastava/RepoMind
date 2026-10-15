package openai

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/repomind/repomind/internal/application/llm"
)

// This file holds the adapter-private typed errors used to drive the
// retry loop. The public, port-level sentinels (llm.ErrRateLimited,
// llm.ErrUnauthorized, …) live in internal/application/llm — these
// types Unwrap into them so callers' errors.Is checks still work.
//
// The motivation for typed errors over plain wrapped sentinels is the
// Retry-After value carried by a 429 response: parsing it out of an
// error.Error() string at retry time would couple the retry loop to
// the exact format produced by classifyHTTPError. With typed errors
// the value is captured at the source as a time.Duration and queried
// via the retryable interface.

// retryable is implemented by errors that the retry loop is allowed
// to back off on. It exposes the server-suggested wait, when known.
type retryable interface {
	retryAfter() time.Duration
}

// rateLimitErr represents a 429 response. retryAfter is populated from
// the Retry-After header when present (integer seconds only).
type rateLimitErr struct {
	msg string
	ra  time.Duration
}

func (e *rateLimitErr) Error() string {
	if e.ra > 0 {
		return fmt.Sprintf("%s: %s (retry-after=%s)", llm.ErrRateLimited.Error(), e.msg, e.ra)
	}
	return fmt.Sprintf("%s: %s", llm.ErrRateLimited.Error(), e.msg)
}

func (e *rateLimitErr) Unwrap() error             { return llm.ErrRateLimited }
func (e *rateLimitErr) retryAfter() time.Duration { return e.ra }

// serverErr represents an HTTP 5xx response from the provider. There
// is intentionally no public sentinel for this category — after the
// retry budget is spent, the wrapped error bubbles up opaquely.
type serverErr struct {
	status int
	msg    string
}

func (e *serverErr) Error() string {
	return fmt.Sprintf("openai: server error %d: %s", e.status, e.msg)
}

func (e *serverErr) retryAfter() time.Duration { return 0 }

// parseRetryAfter handles the integer-seconds form of the Retry-After
// header. HTTP-date form is intentionally not supported — providers
// rarely use it for 429s, and the deterministic exponential-backoff
// fallback in client.go covers that case anyway.
func parseRetryAfter(h string) time.Duration {
	if h = strings.TrimSpace(h); h == "" {
		return 0
	}
	v, err := strconv.Atoi(h)
	if err != nil || v <= 0 {
		return 0
	}
	return time.Duration(v) * time.Second
}
