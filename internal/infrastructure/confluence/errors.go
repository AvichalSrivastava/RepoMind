package confluence

import "errors"

// Sentinel errors mapped from Confluence HTTP status codes. The
// application's publish package re-exports equivalent sentinels;
// publisher.go translates between the two so application code does
// not need to import the confluence package to discriminate failures.
//
// Raw response bodies are intentionally not embedded (PII, tokens).
var (
	ErrUnauthorized = errors.New("confluence: unauthorized")
	ErrForbidden    = errors.New("confluence: forbidden")
	ErrNotFound     = errors.New("confluence: page not found")
	ErrRateLimited  = errors.New("confluence: rate limited")
	ErrServer       = errors.New("confluence: server error")
	ErrConflict     = errors.New("confluence: version conflict")
	ErrBadRequest   = errors.New("confluence: bad request")
)
