package jira

import "errors"

// Sentinel errors mapped from Jira HTTP status codes. Callers should
// use errors.Is to discriminate; the underlying HTTP body is logged
// at the client level but not returned (avoids leaking PII).
var (
	ErrUnauthorized = errors.New("jira: unauthorized")
	ErrForbidden    = errors.New("jira: forbidden")
	ErrNotFound     = errors.New("jira: not found")
	ErrRateLimited  = errors.New("jira: rate limited")
	ErrServer       = errors.New("jira: server error")
	ErrBadRequest   = errors.New("jira: bad request")
)
