package httpserver

import (
	"encoding/json"
	"net/http"
)

// Problem implements RFC 7807 application/problem+json. We expose
// only the subset of fields RepoMind uses: clients can ignore unknown
// fields per the spec.
type Problem struct {
	// Type is a URI reference identifying the problem class. We use
	// "about:blank" because we don't (yet) maintain a problem
	// taxonomy, which is explicitly allowed by §4.2.
	Type string `json:"type"`
	// Title is a short, human-readable summary. Should not change
	// between occurrences of the same problem.
	Title string `json:"title"`
	// Status mirrors the HTTP status code for convenience.
	Status int `json:"status"`
	// Detail is a human-readable explanation specific to this
	// occurrence. May be empty.
	Detail string `json:"detail,omitempty"`
	// Instance is a URI reference identifying the specific
	// occurrence. We set it to the request path so operators can
	// correlate with access logs.
	Instance string `json:"instance,omitempty"`
	// RequestID echoes X-Request-ID so support tickets can quote it.
	// Not a 7807 field — extension fields are allowed by §3.2.
	RequestID string `json:"request_id,omitempty"`
}

// WriteProblem serializes a Problem to w with the right headers.
// Callers MUST NOT also write to w after calling this.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	p := Problem{
		Type:      "about:blank",
		Title:     title,
		Status:    status,
		Detail:    detail,
		Instance:  r.URL.Path,
		RequestID: requestIDFromContext(r.Context()),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	// Best-effort encode — if this fails the connection is hosed
	// anyway and the response code is already on the wire.
	_ = json.NewEncoder(w).Encode(p)
}
