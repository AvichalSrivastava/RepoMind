// Package orchestration owns the application-layer use cases that
// glue adapters together to deliver RepoMind's end-to-end behaviour.
//
// In M9, only the *enqueue* side of orchestration is implemented:
// HTTP handlers and the M10 ticker convert high-level requests
// ("a GitHub webhook arrived", "regenerate release notes for sprint X")
// into durable outbox rows via the Enqueuer port. The actual work —
// fetching the sprint, listing PRs, running analyzers, publishing to
// Confluence — is the use case that lands in M10 and consumes these
// outbox rows.
//
// Separating "produce work item" (M9) from "execute work item" (M10)
// is what makes the system durable: a webhook delivery survives a
// crash because the work item is already in Postgres before we ACK
// the HTTP request, and replays are idempotent because every entry
// point supplies a dedup key.
package orchestration

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// EventType enumerates the outbox event_type values M9 produces. M10's
// worker discriminates on this value to choose the right use case.
type EventType string

const (
	// EventGithubWebhookReceived is enqueued by the webhook handler
	// after HMAC + allow-list checks pass. Aggregate is the webhook
	// delivery; dedup_key is the X-GitHub-Delivery UUID.
	EventGithubWebhookReceived EventType = "github.webhook.received"

	// EventReleaseNotesRequested is enqueued by the operator API and
	// by the M10 sprint-state ticker. Aggregate is the sprint; dedup
	// key is caller-supplied (Idempotency-Key header) or synthesised
	// from sprint_id + reason + minute-bucket for the ticker.
	EventReleaseNotesRequested EventType = "release_notes.requested"

	// EventPRAnalysisRequested is enqueued by the M10 webhook
	// handler (Phase 3) after deciding a PR event warrants risk + QA
	// analysis. Aggregate is the PR; dedup key is
	// "pr-analysis:<owner>/<name>#<number>@<head_sha>" so:
	//   - the same delivery re-played by GitHub collapses,
	//   - a force-push (head_sha changes) triggers a fresh analysis.
	EventPRAnalysisRequested EventType = "pr.analysis.requested"
)

// Aggregate-type tags stored on outbox rows. Informational — they
// drive dashboards and debugging, not dispatch routing.
const (
	AggregateWebhook     = "webhook"
	AggregateSprint      = "sprint"
	AggregatePullRequest = "pull_request"
)

// RepoRef is the owner/name pair we accept from inbound webhooks.
// Kept here (not borrowed from internal/application/github) so the
// orchestration package has no dependency on the github adapter port.
type RepoRef struct {
	Owner string
	Name  string
}

// GithubWebhookInput is the data the webhook handler hands to the
// enqueuer after HMAC + allow-list validation.
type GithubWebhookInput struct {
	// DeliveryID is the X-GitHub-Delivery header value (UUID). MUST
	// be non-empty: it doubles as the outbox dedup_key.
	DeliveryID string
	// Event is the X-GitHub-Event header (e.g. "pull_request").
	Event string
	// Repo identifies the source repository (already allow-list
	// validated by the handler).
	Repo RepoRef
	// RawPayload is the verbatim JSON body. Stored as-is so the M10
	// worker can decode it against any future schema.
	RawPayload json.RawMessage
}

// ReleaseNotesRequestInput is the data the operator API hands to the
// enqueuer after bearer-auth validation.
type ReleaseNotesRequestInput struct {
	// SprintID is the target sprint (path parameter).
	SprintID uuid.UUID
	// IdempotencyKey is the caller-supplied Idempotency-Key header.
	// Empty = caller opts out of dedup (each request enqueues a new
	// row); non-empty = retries of the same key become no-ops.
	IdempotencyKey string
	// Reason narrates *why* the request was made. Stored on the
	// outbox payload for audit; not used for routing. Allowed:
	// "operator", "ticker".
	Reason string
	// Force, when true, instructs the handler to REPLACE existing
	// ticket rows on the Confluence page instead of skipping them.
	// Reserved for backfills (e.g. after adding the QA Owner
	// column). Operator hand-edits to affected rows are lost.
	Force bool
}

// EnqueueResult is what the enqueuer returns. DedupHit=true means a
// prior row with the same dedup_key already existed and OutboxID
// points at that prior row — no new work was created. Callers should
// translate this into an idempotent HTTP response (200 OK instead of
// 202 Accepted) so clients see consistent results across retries.
type EnqueueResult struct {
	OutboxID uuid.UUID
	DedupHit bool
}

// Enqueuer is the M9 boundary into the durable outbox. The methods
// exist because the entry points have different inputs and different
// dedup-key sources; all end up as outbox rows the M10 worker drains
// in arrival order.
//
// Implementations MUST be safe for concurrent use.
type Enqueuer interface {
	EnqueueGithubWebhook(ctx context.Context, in GithubWebhookInput) (*EnqueueResult, error)
	EnqueueReleaseNotesRequest(ctx context.Context, in ReleaseNotesRequestInput) (*EnqueueResult, error)

	// EnqueuePRAnalysisRequest is called by the M10 webhook handler
	// (Phase 3) when a pull_request event with an analysable action
	// arrives. The dedup_key is synthesised from (owner, name,
	// number, head_sha) so the SAME head SHA is analysed once and
	// only once, but a force-push re-triggers analysis. Returns
	// DedupHit=true on retries.
	EnqueuePRAnalysisRequest(ctx context.Context, in PRAnalysisRequestInput) (*EnqueueResult, error)
}

// ---- payloads ---------------------------------------------------------------
//
// The payload structs below are the on-the-wire shape of outbox.payload
// for each EventType. They live here (application layer) rather than
// in the worker so the producer and consumer share a single source of
// truth.

// GithubWebhookPayload is what M9 writes for EventGithubWebhookReceived.
type GithubWebhookPayload struct {
	DeliveryID string          `json:"delivery_id"`
	Event      string          `json:"event"`
	Repo       RepoRef         `json:"repo"`
	RawPayload json.RawMessage `json:"raw_payload"`
}

// ReleaseNotesRequestPayload is what M9 writes for
// EventReleaseNotesRequested.
type ReleaseNotesRequestPayload struct {
	SprintID uuid.UUID `json:"sprint_id"`
	Reason   string    `json:"reason"`
	// Force mirrors ReleaseNotesRequestInput.Force -- when true,
	// the handler passes ForceRefresh=true to the publisher so
	// existing rows are replaced rather than skipped.
	Force bool `json:"force,omitempty"`
}

// ---- PR analysis (Phase 3 -> Phase 4) -------------------------------------

// PRAnalysisRequestInput is what the webhook handler hands to the
// enqueuer when a pull_request event with an analysable action
// arrives. All fields are required.
type PRAnalysisRequestInput struct {
	// Repo identifies the source repository (already allow-list
	// validated by the webhook handler upstream).
	Repo RepoRef
	// Number is the PR number within the repository (#1, #2, ...).
	Number int
	// HeadSHA is the commit SHA at the tip of the PR branch when
	// the webhook fired. Doubles as the dedup discriminator so a
	// force-push triggers a re-analysis but a stuck retry does not.
	HeadSHA string
	// Action is the GitHub webhook action ("opened",
	// "synchronize", ...). Informational; stored on the payload
	// for audit.
	Action string
	// DeliveryID is the X-GitHub-Delivery UUID of the originating
	// webhook. Used to correlate the analysis row back to the
	// webhook row for tracing.
	DeliveryID string
}

// PRAnalysisRequestPayload is what the enqueuer writes for
// EventPRAnalysisRequested. The Phase 4 handler decodes this to know
// which PR to fetch + analyse.
type PRAnalysisRequestPayload struct {
	Repo       RepoRef `json:"repo"`
	Number     int     `json:"number"`
	HeadSHA    string  `json:"head_sha"`
	Action     string  `json:"action"`
	DeliveryID string  `json:"delivery_id"`
}
