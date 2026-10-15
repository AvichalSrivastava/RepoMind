package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	apigithub "github.com/repomind/repomind/internal/application/github"
	"github.com/repomind/repomind/internal/application/orchestration"
)

// maxWebhookBodyBytes caps the inbound webhook body. GitHub's
// documented maximum is 25 MB; we mirror it exactly so legitimate
// large pushes (rare but real) get accepted and oversized payloads
// fail fast with a 413 instead of consuming pool capacity.
const maxWebhookBodyBytes = 25 * 1024 * 1024

// webhookHandler validates an inbound GitHub webhook and enqueues it
// for asynchronous processing.
//
// Contract (in order, fast-fail on the first violation):
//
//  1. POST only — the router pins the method, this is belt-and-braces.
//  2. Body ≤ 25 MB (http.MaxBytesReader). Oversized: 413.
//  3. X-GitHub-Delivery present and non-empty. Missing: 400.
//  4. X-GitHub-Event present. Missing: 400.
//  5. HMAC verify against the raw body. Bad: 401.
//  6. Repo allow-list: parse minimal JSON for repository.full_name,
//     compare against the configured set. Reject unknown: 403.
//  7. (NEW M10) Branch policy: when event is pull_request and a
//     BranchPolicy is configured, peek pull_request.base.ref and
//     verify the PR targets an in-scope branch. Reject with 204
//     (acknowledge to GitHub so it stops retrying) + structured log.
//  8. Enqueue via orchestration; the dedup key is the delivery id, so
//     GitHub retries collapse. Response: 202 (accepted), with the
//     outbox id in the body for traceability. Dedup hit returns 200.
//
// We respond with the outbox id so operators can grep logs and join
// against the durable record. We do *not* return any GitHub-supplied
// data — defense in depth against echoing attacker-controlled bytes.
type webhookHandler struct {
	verifier     apigithub.WebhookVerifier
	enqueuer     orchestration.Enqueuer
	allowedRepos map[string]struct{}    // "owner/name" lower-cased
	branchPolicy apigithub.BranchPolicy // optional; nil = no filter
	logger       *slog.Logger
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// (1) method check
	if r.Method != http.MethodPost {
		WriteProblem(w, r, http.StatusMethodNotAllowed, "method not allowed", "use POST")
		return
	}

	// (2) body cap. MaxBytesReader writes the 413 if the limit
	// trips during ReadAll; we map any I/O error to a Problem.
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			WriteProblem(w, r, http.StatusRequestEntityTooLarge,
				"payload too large", "webhook body exceeds 25 MB")
			return
		}
		WriteProblem(w, r, http.StatusBadRequest, "bad request", "could not read body")
		return
	}

	// (3) + (4) required headers
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	if deliveryID == "" {
		WriteProblem(w, r, http.StatusBadRequest, "bad request",
			"missing X-GitHub-Delivery header")
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "" {
		WriteProblem(w, r, http.StatusBadRequest, "bad request",
			"missing X-GitHub-Event header")
		return
	}

	// (5) HMAC verification — MUST run over the unmodified body.
	if err := h.verifier.Verify(r.Header.Get("X-Hub-Signature-256"), body); err != nil {
		// Deliberately uniform message: an attacker shouldn't be
		// able to distinguish "no sig" from "bad sig" via wording.
		WriteProblem(w, r, http.StatusUnauthorized, "unauthorized",
			"webhook signature verification failed")
		return
	}

	// (6) allow-list. We only need {repository:{full_name:""}};
	// parse a minimal envelope to avoid trusting the full payload.
	var env struct {
		Repository struct {
			FullName string `json:"full_name"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
			Name string `json:"name"`
		} `json:"repository"`
		// pull_request.base.ref is needed for step (7) -- the branch
		// policy check. Only populated for pull_request events; other
		// event types ignore this field.
		PullRequest struct {
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "bad request",
			"webhook payload is not valid JSON")
		return
	}
	full := env.Repository.FullName
	if full == "" && env.Repository.Owner.Login != "" && env.Repository.Name != "" {
		full = env.Repository.Owner.Login + "/" + env.Repository.Name
	}
	if full == "" {
		// "ping" deliveries from GitHub legitimately have no
		// repository — accept them so the operator can confirm
		// the integration. They don't enqueue any work.
		if event == "ping" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		WriteProblem(w, r, http.StatusBadRequest, "bad request",
			"webhook payload missing repository.full_name")
		return
	}
	if _, ok := h.allowedRepos[normalizeRepo(full)]; !ok {
		WriteProblem(w, r, http.StatusForbidden, "forbidden",
			"repository is not on the allow-list")
		return
	}

	// (7) branch policy -- only meaningful for pull_request events.
	// nil policy = no filter (legacy behaviour).
	if h.branchPolicy != nil && event == "pull_request" {
		base := env.PullRequest.Base.Ref
		if base != "" && !h.branchPolicy.Accept(env.Repository.Owner.Login, env.Repository.Name, base) {
			// Acknowledge with 204 so GitHub considers the
			// delivery successful and stops retrying. The decision
			// to drop is captured in the access log so operators
			// can audit later.
			if h.logger != nil {
				h.logger.LogAttrs(r.Context(), slog.LevelInfo,
					"webhook: rejected by branch policy",
					slog.String("delivery_id", deliveryID),
					slog.String("repo", full),
					slog.String("target_branch", base),
				)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	// (8) enqueue.
	res, err := h.enqueuer.EnqueueGithubWebhook(r.Context(), orchestration.GithubWebhookInput{
		DeliveryID: deliveryID,
		Event:      event,
		Repo: orchestration.RepoRef{
			Owner: env.Repository.Owner.Login,
			Name:  env.Repository.Name,
		},
		RawPayload: body,
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
		"outbox_id": res.OutboxID,
		"duplicate": res.DedupHit,
	})
}

// writeJSON is a tiny helper to ensure we always set Content-Type
// before WriteHeader. The encoder error is unrecoverable at this
// point (headers already sent), so we swallow it.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// normalizeRepo lowercases an "owner/name" pair. GitHub usernames
// and repository names are case-insensitive on lookup but the
// payload preserves the original casing — normalising here keeps the
// allow-list robust against operator typos.
func normalizeRepo(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// allowedRepoSet builds the lookup map used by the webhook handler
// from the parsed `[]github.RepoSpec` we get from the config layer.
// It lives here so the server constructor doesn't need to know about
// the slice shape.
func allowedRepoSet(specs []apigithub.RepositoryRef) map[string]struct{} {
	m := make(map[string]struct{}, len(specs))
	for _, s := range specs {
		m[normalizeRepo(s.Owner+"/"+s.Name)] = struct{}{}
	}
	return m
}
