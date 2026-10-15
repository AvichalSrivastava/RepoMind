package httpserver

import (
	"context"
	"net/http"
	"time"
)

// HealthChecker is the contract /readyz probes against. Implementations
// SHOULD do the cheapest possible check that proves they can serve
// (a single SELECT 1 for Postgres). 1s deadline is enforced here.
type HealthChecker interface {
	Ping(ctx context.Context) error
}

type healthHandler struct {
	db HealthChecker
}

// liveness — process is running and the HTTP loop responds. Used by
// orchestrators to decide whether to restart. Never depends on
// downstreams.
func (h *healthHandler) liveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// readiness — process can serve traffic. We probe the DB pool
// because a healthy DB is a hard prerequisite for *every* endpoint
// (the outbox lives there). A failed probe returns 503 so the
// load balancer takes us out of rotation.
func (h *healthHandler) readiness(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		// Defensive: if no checker was wired, we can't claim
		// readiness. Treat as "not ready" rather than "ready".
		WriteProblem(w, r, http.StatusServiceUnavailable,
			"not ready", "database probe is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := h.db.Ping(ctx); err != nil {
		WriteProblem(w, r, http.StatusServiceUnavailable,
			"not ready", "database probe failed: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}
