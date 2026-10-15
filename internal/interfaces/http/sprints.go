package httpserver

import (
	"net/http"
	"strings"

	"github.com/repomind/repomind/internal/application/orchestration"
	"github.com/repomind/repomind/internal/domain/shared"
)

// sprintsHandler hosts the operator-facing surface for sprint-scoped
// actions. Today: regenerate release notes (asynchronous).
//
// Authentication is enforced upstream by bearerAuthMiddleware — this
// handler runs only after the bearer key matched.
//
// Idempotency: if the caller sends Idempotency-Key, retries collapse
// to a single outbox row and the response carries duplicate=true.
// Without the header, every call enqueues a new row (caller opts out).
type sprintsHandler struct {
	enqueuer orchestration.Enqueuer
}

// releaseNotes handles POST /api/v1/sprints/{id}/release-notes.
//
// Path-param parsing uses the Go 1.22+ ServeMux PathValue API.
func (h *sprintsHandler) releaseNotes(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("id")
	sprintID, err := shared.ParseSprintID(raw)
	if err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "bad request",
			"sprint id must be a valid UUID: "+err.Error())
		return
	}

	// Parse-and-validate the raw header on the way in. Empty is OK
	// (caller opts out of dedup); non-empty must be a printable
	// ASCII subset we're comfortable storing.
	idemKey := r.Header.Get("Idempotency-Key")
	if !validIdempotencyKey(idemKey) {
		WriteProblem(w, r, http.StatusBadRequest, "bad request",
			"Idempotency-Key must be 1..200 printable ASCII chars")
		return
	}

	// Optional ?force=true tells the handler to REPLACE existing
	// ticket rows on the Confluence page (backfill mode) rather
	// than skipping them. Operator hand-edits are lost.
	force := false
	if v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("force"))); v != "" {
		switch v {
		case "1", "true", "yes":
			force = true
		case "0", "false", "no":
			force = false
		default:
			WriteProblem(w, r, http.StatusBadRequest, "bad request",
				"query param 'force' must be true or false")
			return
		}
	}

	res, err := h.enqueuer.EnqueueReleaseNotesRequest(r.Context(),
		orchestration.ReleaseNotesRequestInput{
			SprintID:       sprintID.UUID(),
			IdempotencyKey: idemKey,
			Reason:         "operator",
			Force:          force,
		})
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError,
			"enqueue failed", err.Error())
		return
	}

	status := http.StatusAccepted
	if res.DedupHit {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"sprint_id": sprintID.String(),
		"outbox_id": res.OutboxID,
		"duplicate": res.DedupHit,
	})
}

// validIdempotencyKey accepts empty (opt-out) and otherwise enforces
// a conservative shape: 1..200 chars, every byte must be printable
// ASCII excluding control chars and quote marks. Stricter than RFC
// drafts, but our use case doesn't need binary keys and a tighter
// rule keeps the dedup_key column harmless when displayed in logs.
func validIdempotencyKey(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > 200 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x21 || c > 0x7E || c == '"' || c == '\'' || c == '`' {
			return false
		}
	}
	return true
}
