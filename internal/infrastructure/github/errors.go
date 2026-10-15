package github

import "errors"

// Sentinel errors returned by the GitHub adapter. Use errors.Is to
// match them in callers — the adapter wraps every API failure to one
// of these.
var (
	// ErrUnauthorized indicates a 401 from the API — usually a bad
	// installation token, expired App key, or revoked App.
	ErrUnauthorized = errors.New("github: unauthorized")

	// ErrForbidden indicates a 403 — App lacks the permission, or the
	// installation cannot see this repo.
	ErrForbidden = errors.New("github: forbidden")

	// ErrNotFound indicates a 404 — repo/PR/file does not exist or is
	// invisible to the installation.
	ErrNotFound = errors.New("github: not found")

	// ErrRateLimited indicates a primary or secondary rate limit hit
	// (429 or 403 + X-RateLimit-Remaining: 0).
	ErrRateLimited = errors.New("github: rate limited")

	// ErrServer is any 5xx from GitHub.
	ErrServer = errors.New("github: server error")

	// ErrBadRequest indicates a 4xx (other than the above) — usually a
	// programmer error in the adapter.
	ErrBadRequest = errors.New("github: bad request")

	// ErrUnauthorizedRepo means the caller asked for a repo that was
	// not declared in Config.Repos. Fails closed at the adapter
	// boundary regardless of App permissions.
	ErrUnauthorizedRepo = errors.New("github: repo not allow-listed")

	// ErrInvalidSignature means a webhook payload failed HMAC
	// verification. Never log the body or the signature when this
	// is returned.
	ErrInvalidSignature = errors.New("github: invalid webhook signature")
)
