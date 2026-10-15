package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/repomind/repomind/internal/application/persistence"
)

// OutboxEnqueuer is the default Enqueuer implementation. It wraps a
// persistence.Outbox and translates input DTOs into OutboxEvent rows.
// All event production happens through this type so any future
// observability (metrics, structured logs) lands in one place.
type OutboxEnqueuer struct {
	outbox persistence.Outbox
}

// NewOutboxEnqueuer wires an Enqueuer backed by the persistence Outbox.
// outbox is required; a nil value is a programming error and panics.
func NewOutboxEnqueuer(outbox persistence.Outbox) *OutboxEnqueuer {
	if outbox == nil {
		panic("orchestration: outbox is required")
	}
	return &OutboxEnqueuer{outbox: outbox}
}

// Compile-time guarantee.
var _ Enqueuer = (*OutboxEnqueuer)(nil)

// EnqueueGithubWebhook turns a verified webhook delivery into an
// outbox row. The X-GitHub-Delivery header is the dedup key so GitHub
// retries collapse to a single row.
func (e *OutboxEnqueuer) EnqueueGithubWebhook(
	ctx context.Context,
	in GithubWebhookInput,
) (*EnqueueResult, error) {
	if err := validateWebhookInput(in); err != nil {
		return nil, err
	}

	payload := GithubWebhookPayload(in)
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("orchestration: marshal webhook payload: %w", err)
	}

	evt := persistence.OutboxEvent{
		AggregateType: AggregateWebhook,
		// Use the delivery UUID as the aggregate id when it parses;
		// fall back to a fresh UUID otherwise. The delivery id
		// header is documented as a UUID by GitHub, but we don't
		// want a malformed header to fail the whole enqueue path
		// when we've already accepted the body.
		AggregateID: parseUUIDOrNew(in.DeliveryID),
		EventType:   string(EventGithubWebhookReceived),
		Payload:     raw,
		DedupKey:    in.DeliveryID,
	}

	res, err := e.outbox.EnqueueWithDedup(ctx, evt)
	if err != nil {
		return nil, fmt.Errorf("orchestration: enqueue webhook: %w", err)
	}
	return &EnqueueResult{OutboxID: res.ID, DedupHit: res.DedupHit}, nil
}

// EnqueueReleaseNotesRequest turns an operator/ticker request into an
// outbox row. If IdempotencyKey is supplied, dedup is enforced; with
// no key we fall back to a fresh-row Enqueue (no dedup, every call
// produces a new row).
func (e *OutboxEnqueuer) EnqueueReleaseNotesRequest(
	ctx context.Context,
	in ReleaseNotesRequestInput,
) (*EnqueueResult, error) {
	if err := validateReleaseNotesInput(in); err != nil {
		return nil, err
	}

	payload := ReleaseNotesRequestPayload{
		SprintID: in.SprintID,
		Reason:   in.Reason,
		Force:    in.Force,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("orchestration: marshal release-notes payload: %w", err)
	}

	evt := persistence.OutboxEvent{
		AggregateType: AggregateSprint,
		AggregateID:   in.SprintID,
		EventType:     string(EventReleaseNotesRequested),
		Payload:       raw,
		DedupKey:      in.IdempotencyKey,
	}

	if in.IdempotencyKey == "" {
		if err := e.outbox.Enqueue(ctx, evt); err != nil {
			return nil, fmt.Errorf("orchestration: enqueue release-notes: %w", err)
		}
		return &EnqueueResult{OutboxID: evt.ID, DedupHit: false}, nil
	}

	res, err := e.outbox.EnqueueWithDedup(ctx, evt)
	if err != nil {
		return nil, fmt.Errorf("orchestration: enqueue release-notes: %w", err)
	}
	return &EnqueueResult{OutboxID: res.ID, DedupHit: res.DedupHit}, nil
}

// validateWebhookInput enforces the adapter-boundary invariants the
// HTTP handler should already have established. Cheap to re-check.
func validateWebhookInput(in GithubWebhookInput) error {
	if strings.TrimSpace(in.DeliveryID) == "" {
		return errors.New("orchestration: webhook delivery_id is required")
	}
	if strings.TrimSpace(in.Event) == "" {
		return errors.New("orchestration: webhook event is required")
	}
	if strings.TrimSpace(in.Repo.Owner) == "" || strings.TrimSpace(in.Repo.Name) == "" {
		return errors.New("orchestration: webhook repo owner/name is required")
	}
	if len(in.RawPayload) == 0 {
		return errors.New("orchestration: webhook raw_payload is required")
	}
	return nil
}

func validateReleaseNotesInput(in ReleaseNotesRequestInput) error {
	if in.SprintID == uuid.Nil {
		return errors.New("orchestration: sprint_id is required")
	}
	switch strings.TrimSpace(in.Reason) {
	case "operator", "ticker":
	default:
		return fmt.Errorf("orchestration: reason must be operator|ticker, got %q", in.Reason)
	}
	return nil
}

// EnqueuePRAnalysisRequest writes a pr.analysis.requested outbox row.
// The dedup key is "pr-analysis:<owner>/<name>#<number>@<head_sha>",
// so retries of the same head SHA collapse, but a force-push (which
// changes head_sha) re-triggers analysis.
func (e *OutboxEnqueuer) EnqueuePRAnalysisRequest(
	ctx context.Context,
	in PRAnalysisRequestInput,
) (*EnqueueResult, error) {
	if err := validatePRAnalysisInput(in); err != nil {
		return nil, err
	}

	payload := PRAnalysisRequestPayload(in)
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("orchestration: marshal pr-analysis payload: %w", err)
	}

	evt := persistence.OutboxEvent{
		// AggregateID has no natural UUID for a PR webhook (PRs
		// are identified by repo+number externally); use a fresh
		// one. The DeliveryID is preserved on the payload for
		// tracing back to the originating webhook.
		AggregateType: AggregatePullRequest,
		AggregateID:   uuid.New(),
		EventType:     string(EventPRAnalysisRequested),
		Payload:       raw,
		DedupKey:      prAnalysisDedupKey(in),
	}

	res, err := e.outbox.EnqueueWithDedup(ctx, evt)
	if err != nil {
		return nil, fmt.Errorf("orchestration: enqueue pr-analysis: %w", err)
	}
	return &EnqueueResult{OutboxID: res.ID, DedupHit: res.DedupHit}, nil
}

func validatePRAnalysisInput(in PRAnalysisRequestInput) error {
	if strings.TrimSpace(in.Repo.Owner) == "" || strings.TrimSpace(in.Repo.Name) == "" {
		return errors.New("orchestration: pr-analysis repo owner/name is required")
	}
	if in.Number <= 0 {
		return fmt.Errorf("orchestration: pr-analysis number must be > 0, got %d", in.Number)
	}
	if strings.TrimSpace(in.HeadSHA) == "" {
		return errors.New("orchestration: pr-analysis head_sha is required")
	}
	if strings.TrimSpace(in.Action) == "" {
		return errors.New("orchestration: pr-analysis action is required")
	}
	if strings.TrimSpace(in.DeliveryID) == "" {
		return errors.New("orchestration: pr-analysis delivery_id is required")
	}
	return nil
}

// prAnalysisDedupKey synthesises the dedup key. Keeping it in one
// place means producers and (future) inspectors agree on the format.
func prAnalysisDedupKey(in PRAnalysisRequestInput) string {
	return fmt.Sprintf("pr-analysis:%s/%s#%d@%s",
		in.Repo.Owner, in.Repo.Name, in.Number, in.HeadSHA)
}

func parseUUIDOrNew(s string) uuid.UUID {
	if u, err := uuid.Parse(s); err == nil {
		return u
	}
	return uuid.New()
}

// Persisted is a convenience type alias to make signatures readable
// when the worker (M10) re-imports the payload struct. Kept here so
// any future field renames are caught at compile time on both sides.
type Persisted = persistence.OutboxEvent
