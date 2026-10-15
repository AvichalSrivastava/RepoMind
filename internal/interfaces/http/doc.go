// Package httpserver is the HTTP interfaces layer for RepoMind. It
// owns the inbound REST + webhook surface that turns external
// requests into durable outbox rows via the orchestration enqueuer.
//
// Boundaries:
//
//   - This package depends on internal/application (ports + use cases)
//     and the stdlib only. It MUST NOT import any infrastructure
//     adapter (postgres, openai, github) — adapters are wired into it
//     by the composition root in cmd/repomind.
//
//   - Handlers are thin: validate input, call an application port,
//     translate the result to an HTTP response. All business decisions
//     live in the application layer.
//
//   - All error responses follow RFC 7807 (application/problem+json).
//
//   - Logging uses log/slog. Every request gets a request_id (header
//     X-Request-ID, generated if missing) that flows into structured
//     log fields and is echoed back on the response so operators can
//     correlate.
//
//   - Authentication: webhook routes verify HMAC; operator routes use
//     a static bearer API key (constant-time compare). When the key
//     env var is empty, operator routes are not registered at all —
//     the binary refuses to expose privileged surface unauthenticated.
package httpserver
