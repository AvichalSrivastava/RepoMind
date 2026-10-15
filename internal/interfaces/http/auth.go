package httpserver

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerAuthMiddleware enforces `Authorization: Bearer <key>`.
//
// Comparison is constant-time so an attacker cannot recover the key
// byte-by-byte via response timing. A length mismatch is *also*
// constant-time (subtle.ConstantTimeCompare returns 0 when lengths
// differ without short-circuiting); we still pad to avoid leaking
// the configured key's length through error timing.
//
// If apiKey is empty, the middleware refuses every request — callers
// SHOULD avoid registering the route at all in that case (this is a
// belt-and-braces guard).
func bearerAuthMiddleware(apiKey string) func(http.Handler) http.Handler {
	expected := []byte(apiKey)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(expected) == 0 {
				WriteProblem(w, r, http.StatusUnauthorized,
					"unauthorized", "operator API is disabled")
				return
			}
			h := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(h, prefix) {
				WriteProblem(w, r, http.StatusUnauthorized,
					"unauthorized", "missing bearer token")
				return
			}
			got := []byte(strings.TrimSpace(h[len(prefix):]))
			// Pad got to len(expected) so the compare always
			// runs over the same byte count regardless of input.
			if len(got) != len(expected) {
				pad := make([]byte, len(expected))
				if subtle.ConstantTimeCompare(pad, expected) == 1 {
					// impossible: expected is non-empty
					_ = pad
				}
				WriteProblem(w, r, http.StatusUnauthorized,
					"unauthorized", "invalid bearer token")
				return
			}
			if subtle.ConstantTimeCompare(got, expected) != 1 {
				WriteProblem(w, r, http.StatusUnauthorized,
					"unauthorized", "invalid bearer token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
