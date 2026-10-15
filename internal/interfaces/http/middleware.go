package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
)

// ctxKey is unexported to prevent collisions with other packages.
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota + 1
)

// requestIDHeader is the canonical inbound + outbound header. We
// accept whatever the caller sends (trusted boundary: typically an
// ingress that already deduplicates) and only mint our own if absent.
const requestIDHeader = "X-Request-ID"

// requestIDFromContext returns the request id stored in ctx by
// requestIDMiddleware, or "" if there isn't one.
func requestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// requestIDMiddleware ensures every request carries a stable id we
// can correlate logs by. UUIDv4 is overkill for this use case but
// it's free and never collides.
func requestIDMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(requestIDHeader)
			if id == "" {
				id = uuid.NewString()
			}
			w.Header().Set(requestIDHeader, id)
			ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// statusRecorder wraps a ResponseWriter to capture the eventual
// status code for the access log. We only need status + size, so
// minimal indirection — no hijacker / pusher pass-through needed for
// a JSON API.
type statusRecorder struct {
	http.ResponseWriter
	status int
	size   int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.status = code
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	n, err := s.ResponseWriter.Write(b)
	s.size += n
	return n, err
}

// loggingMiddleware emits a single structured log line per request,
// after the handler returns. We log at info; non-2xx flips it to warn
// so operators can grep cheaply.
func loggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			lvl := slog.LevelInfo
			if rec.status >= 500 {
				lvl = slog.LevelError
			} else if rec.status >= 400 {
				lvl = slog.LevelWarn
			}
			logger.LogAttrs(r.Context(), lvl, "http_request",
				slog.String("request_id", requestIDFromContext(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("remote", r.RemoteAddr),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.size),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}

// recoverMiddleware turns panics into 500 problem responses + an
// error-level log line including the stack. Must be the outermost
// middleware so it can catch panics from every layer below.
func recoverMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rv := recover(); rv != nil {
					logger.LogAttrs(r.Context(), slog.LevelError, "http_panic",
						slog.String("request_id", requestIDFromContext(r.Context())),
						slog.String("path", r.URL.Path),
						slog.Any("panic", rv),
						slog.String("stack", string(debug.Stack())),
					)
					// Avoid double-write if the handler already
					// flushed headers before panicking.
					WriteProblem(w, r, http.StatusInternalServerError,
						"internal server error", "unexpected server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// chain composes middlewares left-to-right, i.e. chain(h, a, b)
// produces a(b(h)). The first middleware runs first per request.
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	// Apply in reverse so the leftmost mw is the outermost wrapper.
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
