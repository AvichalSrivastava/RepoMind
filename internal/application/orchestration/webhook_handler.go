package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	apigithub "github.com/repomind/repomind/internal/application/github"
	"github.com/repomind/repomind/internal/application/persistence"
)

// WebhookHandler is the dispatcher Handler for
// EventGithubWebhookReceived. Its job is narrow and intentionally
// lightweight: decide whether the webhook represents a pull-request
// event we want to analyse and, if so, enqueue a
// pr.analysis.requested follow-up event for Phase 4 to consume.
//
// The handler does NOT call GitHub, the LLM, or persist any
// aggregate. Keeping it small means:
//
//   - Webhook deliveries are processed at near-zero cost so the
//     dispatcher's batch throughput stays high even during bursty
//     PR activity.
//   - The expensive work (fetching the full PR + running analysers)
//     runs in a separate outbox row that can be retried independently
//     when GitHub or the LLM provider hiccups -- no need to re-deliver
//     the original webhook.
//   - The dedup key on the follow-up event is keyed on head_sha, so
//     re-delivered webhooks for the same SHA do not re-trigger LLM
//     calls; force-pushes correctly do.
//
// Non-pull-request events (push, ping, release, etc.) and PR events
// with non-analysable actions (closed, edited, labeled, ...) are
// accepted with a structured log line and dispatched to no further
// work. The outbox row flips to status='dispatched' so it does not
// linger.
//
// BranchPolicy (optional) is enforced as a defense-in-depth re-check:
// the HTTP layer already filters at ingestion, but a stale outbox
// row from before a policy change should not silently slip through.
// nil policy = no filtering.
type WebhookHandler struct {
	enqueuer     Enqueuer
	logger       *slog.Logger
	branchPolicy apigithub.BranchPolicy
}

// NewWebhookHandler validates deps and returns a Handler. enqueuer
// and logger are required; branchPolicy is optional.
func NewWebhookHandler(enqueuer Enqueuer, logger *slog.Logger, policy apigithub.BranchPolicy) (*WebhookHandler, error) {
	if enqueuer == nil {
		return nil, errors.New("orchestration: WebhookHandler: enqueuer is required")
	}
	if logger == nil {
		return nil, errors.New("orchestration: WebhookHandler: logger is required")
	}
	return &WebhookHandler{enqueuer: enqueuer, logger: logger, branchPolicy: policy}, nil
}

// Compile-time check.
var _ Handler = (*WebhookHandler)(nil)

// analysableActions are the pull_request webhook actions that warrant
// fresh risk + QA analysis. Other actions (closed, edited, labeled,
// review_requested, ...) are no-ops at this layer.
//
//   - opened           -- new PR
//   - synchronize      -- new commits pushed
//   - reopened         -- previously closed PR reopened
//   - ready_for_review -- draft -> ready
var analysableActions = map[string]struct{}{
	"opened":           {},
	"synchronize":      {},
	"reopened":         {},
	"ready_for_review": {},
}

// minimalPullRequestEnvelope captures just the fields the handler
// needs to route work. We deliberately do NOT decode the full
// payload: any extra GitHub data is the Phase 4 handler's job.
type minimalPullRequestEnvelope struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
}

// Handle decodes the outbox payload and routes to PR analysis when
// applicable. Returns nil on every "we processed this" outcome
// (including no-op events); returns an error only when something
// genuinely retryable went wrong (enqueue failed, payload corrupt).
func (h *WebhookHandler) Handle(ctx context.Context, evt persistence.OutboxEvent) error {
	if evt.EventType != string(EventGithubWebhookReceived) {
		// Defensive: the dispatcher routes by event_type, so we
		// should never see anything else here. Surface as an
		// error so the dispatcher logs + marks failed -- it's an
		// operator bug.
		return fmt.Errorf("orchestration: WebhookHandler: unexpected event_type=%q", evt.EventType)
	}

	var wh GithubWebhookPayload
	if err := json.Unmarshal(evt.Payload, &wh); err != nil {
		// Malformed payload in the outbox. Not retryable -- the
		// same bad bytes will fail forever. We return the error
		// so the dispatcher counts it as an attempt; after
		// MaxAttempts it is marked permanently failed.
		return fmt.Errorf("orchestration: WebhookHandler: decode payload: %w", err)
	}

	logger := h.logger.With(
		"event_id", evt.ID.String(),
		"delivery_id", wh.DeliveryID,
		"github_event", wh.Event,
		"repo", wh.Repo.Owner+"/"+wh.Repo.Name,
	)

	// Non-PR events are accepted but do no work. Use cases:
	//   - "ping" deliveries from GitHub on App install
	//   - "push", "release", "create" -- the M9 webhook handler
	//     filters none of these out; we acknowledge here.
	if wh.Event != "pull_request" {
		logger.Info("WebhookHandler: non-PR event accepted, no work to enqueue")
		return nil
	}

	var env minimalPullRequestEnvelope
	if err := json.Unmarshal(wh.RawPayload, &env); err != nil {
		// The webhook handler upstream already validated the
		// envelope contained a repository; if pull_request shape
		// is broken, we can't act. Treat as fatal (retry will
		// hit the same bytes).
		return fmt.Errorf("orchestration: WebhookHandler: decode raw_payload: %w", err)
	}

	action := strings.ToLower(strings.TrimSpace(env.Action))
	if _, ok := analysableActions[action]; !ok {
		logger.Info("WebhookHandler: PR action not analysable, no work to enqueue",
			"action", action)
		return nil
	}

	number := env.PullRequest.Number
	if number == 0 {
		number = env.Number // older webhook shape
	}
	headSHA := strings.TrimSpace(env.PullRequest.Head.SHA)
	baseRef := strings.TrimSpace(env.PullRequest.Base.Ref)

	if number <= 0 || headSHA == "" {
		// The webhook is malformed but we filtered it past the
		// upstream HMAC check; we don't want to retry-loop on
		// garbage payload from a real-but-buggy producer.
		logger.Warn("WebhookHandler: PR webhook missing number or head SHA, dropping",
			"action", action, "number", number, "head_sha", headSHA)
		return nil
	}

	// Defense-in-depth branch policy check. The HTTP webhook layer
	// already filtered at ingestion, but a stale outbox row from
	// before a policy change must not silently slip through.
	if h.branchPolicy != nil && baseRef != "" {
		if !h.branchPolicy.Accept(wh.Repo.Owner, wh.Repo.Name, baseRef) {
			logger.Info("WebhookHandler: PR rejected by branch policy, no work to enqueue",
				"action", action, "target_branch", baseRef)
			return nil
		}
	}

	res, err := h.enqueuer.EnqueuePRAnalysisRequest(ctx, PRAnalysisRequestInput{
		Repo:       wh.Repo,
		Number:     number,
		HeadSHA:    headSHA,
		Action:     action,
		DeliveryID: wh.DeliveryID,
	})
	if err != nil {
		// Enqueue failure (typically Postgres hiccup) is
		// retryable -- bubble up so the dispatcher applies
		// backoff.
		return fmt.Errorf("orchestration: WebhookHandler: enqueue pr-analysis: %w", err)
	}

	logger.Info("WebhookHandler: pr-analysis enqueued",
		"action", action,
		"pr_number", number,
		"head_sha", headSHA,
		"follow_up_outbox_id", res.OutboxID,
		"dedup_hit", res.DedupHit,
	)
	return nil
}
