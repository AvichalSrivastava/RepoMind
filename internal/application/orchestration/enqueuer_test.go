package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/repomind/repomind/internal/application/persistence"
)

// fakeOutbox is a minimal in-memory Outbox that exercises both code
// paths the enqueuer uses (Enqueue + EnqueueWithDedup) without
// dragging in a real Postgres dependency.
type fakeOutbox struct {
	mu             sync.Mutex
	byDedup        map[string]uuid.UUID
	enqueueCalls   []persistence.OutboxEvent
	dedupCalls     []persistence.OutboxEvent
	enqueueErr     error
	dedupErr       error
	dedupHitOnNext bool // simulates a UNIQUE collision
	cannedDedupID  uuid.UUID
}

var _ persistence.Outbox = (*fakeOutbox)(nil)

func newFake() *fakeOutbox { return &fakeOutbox{byDedup: map[string]uuid.UUID{}} }

func (f *fakeOutbox) Enqueue(_ context.Context, e persistence.OutboxEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enqueueErr != nil {
		return f.enqueueErr
	}
	f.enqueueCalls = append(f.enqueueCalls, e)
	return nil
}

func (f *fakeOutbox) EnqueueWithDedup(
	_ context.Context,
	e persistence.OutboxEvent,
) (persistence.EnqueueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dedupErr != nil {
		return persistence.EnqueueResult{}, f.dedupErr
	}
	if f.dedupHitOnNext {
		f.dedupHitOnNext = false
		return persistence.EnqueueResult{ID: f.cannedDedupID, DedupHit: true}, nil
	}
	if existing, ok := f.byDedup[e.DedupKey]; ok {
		return persistence.EnqueueResult{ID: existing, DedupHit: true}, nil
	}
	id := uuid.New()
	f.byDedup[e.DedupKey] = id
	f.dedupCalls = append(f.dedupCalls, e)
	return persistence.EnqueueResult{ID: id, DedupHit: false}, nil
}

func (f *fakeOutbox) ListPending(context.Context, int) ([]persistence.OutboxEvent, error) {
	return nil, nil
}
func (f *fakeOutbox) MarkDispatched(context.Context, uuid.UUID, time.Time) error { return nil }
func (f *fakeOutbox) MarkFailed(context.Context, uuid.UUID, string, time.Time) error {
	return nil
}
func (f *fakeOutbox) MarkPermanentlyFailed(context.Context, uuid.UUID, string) error {
	return nil
}

func TestEnqueueGithubWebhook_InsertsRow(t *testing.T) {
	t.Parallel()
	fake := newFake()
	enq := NewOutboxEnqueuer(fake)

	in := GithubWebhookInput{
		DeliveryID: "11111111-1111-1111-1111-111111111111",
		Event:      "pull_request",
		Repo:       RepoRef{Owner: "acme", Name: "repo"},
		RawPayload: json.RawMessage(`{"action":"opened"}`),
	}

	res, err := enq.EnqueueGithubWebhook(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.DedupHit)
	assert.NotEqual(t, uuid.Nil, res.OutboxID)

	require.Len(t, fake.dedupCalls, 1)
	row := fake.dedupCalls[0]
	assert.Equal(t, AggregateWebhook, row.AggregateType)
	assert.Equal(t, string(EventGithubWebhookReceived), row.EventType)
	assert.Equal(t, in.DeliveryID, row.DedupKey)

	var payload GithubWebhookPayload
	require.NoError(t, json.Unmarshal(row.Payload, &payload))
	assert.Equal(t, in.Event, payload.Event)
	assert.Equal(t, in.Repo, payload.Repo)
}

func TestEnqueueGithubWebhook_DedupHit(t *testing.T) {
	t.Parallel()
	fake := newFake()
	fake.cannedDedupID = uuid.New()
	fake.dedupHitOnNext = true
	enq := NewOutboxEnqueuer(fake)

	res, err := enq.EnqueueGithubWebhook(context.Background(), GithubWebhookInput{
		DeliveryID: "abc",
		Event:      "ping",
		Repo:       RepoRef{Owner: "o", Name: "n"},
		RawPayload: json.RawMessage(`{}`),
	})
	require.NoError(t, err)
	assert.True(t, res.DedupHit)
	assert.Equal(t, fake.cannedDedupID, res.OutboxID)
}

func TestEnqueueGithubWebhook_Validation(t *testing.T) {
	t.Parallel()
	enq := NewOutboxEnqueuer(newFake())

	cases := map[string]GithubWebhookInput{
		"missing delivery": {Event: "x", Repo: RepoRef{"o", "n"}, RawPayload: json.RawMessage(`{}`)},
		"missing event":    {DeliveryID: "d", Repo: RepoRef{"o", "n"}, RawPayload: json.RawMessage(`{}`)},
		"missing repo":     {DeliveryID: "d", Event: "x", RawPayload: json.RawMessage(`{}`)},
		"missing payload":  {DeliveryID: "d", Event: "x", Repo: RepoRef{"o", "n"}},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := enq.EnqueueGithubWebhook(context.Background(), in)
			assert.Error(t, err)
		})
	}
}

func TestEnqueueReleaseNotesRequest_WithIdempotencyKey(t *testing.T) {
	t.Parallel()
	fake := newFake()
	enq := NewOutboxEnqueuer(fake)

	in := ReleaseNotesRequestInput{
		SprintID:       uuid.New(),
		IdempotencyKey: "op-key-1",
		Reason:         "operator",
	}
	res, err := enq.EnqueueReleaseNotesRequest(context.Background(), in)
	require.NoError(t, err)
	assert.False(t, res.DedupHit)

	// Second call with the same key → dedup hit, same OutboxID.
	res2, err := enq.EnqueueReleaseNotesRequest(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, res2.DedupHit)
	assert.Equal(t, res.OutboxID, res2.OutboxID)

	require.Len(t, fake.dedupCalls, 1) // only one real insert
}

func TestEnqueueReleaseNotesRequest_WithoutKeyTakesPlainPath(t *testing.T) {
	t.Parallel()
	fake := newFake()
	enq := NewOutboxEnqueuer(fake)

	in := ReleaseNotesRequestInput{
		SprintID: uuid.New(),
		Reason:   "ticker",
	}
	_, err := enq.EnqueueReleaseNotesRequest(context.Background(), in)
	require.NoError(t, err)
	require.Len(t, fake.enqueueCalls, 1)
	assert.Len(t, fake.dedupCalls, 0)
}

func TestEnqueueReleaseNotesRequest_BadReason(t *testing.T) {
	t.Parallel()
	enq := NewOutboxEnqueuer(newFake())
	_, err := enq.EnqueueReleaseNotesRequest(context.Background(), ReleaseNotesRequestInput{
		SprintID: uuid.New(),
		Reason:   "manual",
	})
	require.Error(t, err)
}

func TestEnqueueReleaseNotesRequest_OutboxErrorPropagates(t *testing.T) {
	t.Parallel()
	fake := newFake()
	fake.dedupErr = errors.New("boom")
	enq := NewOutboxEnqueuer(fake)

	_, err := enq.EnqueueReleaseNotesRequest(context.Background(), ReleaseNotesRequestInput{
		SprintID:       uuid.New(),
		IdempotencyKey: "k",
		Reason:         "operator",
	})
	require.Error(t, err)
}
