package httpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apigithub "github.com/repomind/repomind/internal/application/github"
	"github.com/repomind/repomind/internal/application/orchestration"
	"github.com/repomind/repomind/internal/infrastructure/config"
	infragithub "github.com/repomind/repomind/internal/infrastructure/github"
)

// ---- shared test doubles -----------------------------------------------

type fakeEnqueuer struct {
	mu                sync.Mutex
	webhookCalls      []orchestration.GithubWebhookInput
	releaseNotesCalls []orchestration.ReleaseNotesRequestInput
	resp              orchestration.EnqueueResult
	err               error
}

var _ orchestration.Enqueuer = (*fakeEnqueuer)(nil)

func (f *fakeEnqueuer) EnqueueGithubWebhook(
	_ context.Context, in orchestration.GithubWebhookInput,
) (*orchestration.EnqueueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.webhookCalls = append(f.webhookCalls, in)
	r := f.resp
	if r.OutboxID == uuid.Nil {
		r.OutboxID = uuid.New()
	}
	return &r, nil
}

func (f *fakeEnqueuer) EnqueueReleaseNotesRequest(
	_ context.Context, in orchestration.ReleaseNotesRequestInput,
) (*orchestration.EnqueueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.releaseNotesCalls = append(f.releaseNotesCalls, in)
	r := f.resp
	if r.OutboxID == uuid.Nil {
		r.OutboxID = uuid.New()
	}
	return &r, nil
}

// EnqueuePRAnalysisRequest is unused by the HTTP layer (the webhook
// handler enqueues plain github.webhook.received; PR-analysis events
// are emitted by the M10 worker's WebhookHandler). Kept here purely
// to satisfy the orchestration.Enqueuer interface contract.
func (f *fakeEnqueuer) EnqueuePRAnalysisRequest(
	_ context.Context, _ orchestration.PRAnalysisRequestInput,
) (*orchestration.EnqueueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	r := f.resp
	if r.OutboxID == uuid.Nil {
		r.OutboxID = uuid.New()
	}
	return &r, nil
}

type fakeHealth struct{ err error }

func (f *fakeHealth) Ping(context.Context) error { return f.err }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestServer(t *testing.T, enq orchestration.Enqueuer, apiKey string, health HealthChecker) *Server {
	t.Helper()
	verifier, err := infragithub.NewHMACVerifier("test-secret")
	require.NoError(t, err)
	srv, err := New(config.HTTP{
		Addr:            ":0",
		ReadTimeout:     5 * time.Second,
		WriteTimeout:    5 * time.Second,
		ShutdownTimeout: 5 * time.Second,
		APIKey:          apiKey,
	}, Deps{
		Logger:          discardLogger(),
		Enqueuer:        enq,
		WebhookVerifier: verifier,
		AllowedRepos: []apigithub.RepositoryRef{
			{Owner: "acme", Name: "repo"},
		},
		HealthChecker: health,
	})
	require.NoError(t, err)
	return srv
}

// signGitHub returns the X-Hub-Signature-256 header value for body.
func signGitHub(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ---- /webhooks/github -------------------------------------------------

func webhookBody(repoFull string) []byte {
	owner, name, _ := strings.Cut(repoFull, "/")
	b, _ := json.Marshal(map[string]any{
		"action": "opened",
		"repository": map[string]any{
			"full_name": repoFull,
			"owner":     map[string]any{"login": owner},
			"name":      name,
		},
	})
	return b
}

func TestWebhook_HappyPathInsert(t *testing.T) {
	t.Parallel()
	enq := &fakeEnqueuer{}
	srv := newTestServer(t, enq, "", nil)
	body := webhookBody("acme/repo")

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", "11111111-1111-1111-1111-111111111111")
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", signGitHub("test-secret", body))

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.Len(t, enq.webhookCalls, 1)
	assert.Equal(t, "pull_request", enq.webhookCalls[0].Event)
	assert.Equal(t, "acme", enq.webhookCalls[0].Repo.Owner)
	assert.Equal(t, "repo", enq.webhookCalls[0].Repo.Name)
}

func TestWebhook_DuplicateReturns200(t *testing.T) {
	t.Parallel()
	enq := &fakeEnqueuer{resp: orchestration.EnqueueResult{DedupHit: true}}
	srv := newTestServer(t, enq, "", nil)
	body := webhookBody("acme/repo")

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", "dup-key")
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", signGitHub("test-secret", body))

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestWebhook_RejectsBadSignature(t *testing.T) {
	t.Parallel()
	enq := &fakeEnqueuer{}
	srv := newTestServer(t, enq, "", nil)
	body := webhookBody("acme/repo")

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", "x")
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	assert.Len(t, enq.webhookCalls, 0)
}

func TestWebhook_RejectsUnknownRepo(t *testing.T) {
	t.Parallel()
	enq := &fakeEnqueuer{}
	srv := newTestServer(t, enq, "", nil)
	body := webhookBody("villain/repo")

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", "x")
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", signGitHub("test-secret", body))

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestWebhook_AcceptsPingWithoutRepo(t *testing.T) {
	t.Parallel()
	enq := &fakeEnqueuer{}
	srv := newTestServer(t, enq, "", nil)
	body := []byte(`{"zen":"non est dolor"}`)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", "ping-id")
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", signGitHub("test-secret", body))

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, enq.webhookCalls, 0)
}

func TestWebhook_RejectsMissingHeaders(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeEnqueuer{}, "", nil)
	body := webhookBody("acme/repo")

	cases := map[string]map[string]string{
		"no delivery": {"X-GitHub-Event": "pull_request"},
		"no event":    {"X-GitHub-Delivery": "x"},
	}
	for name, hdr := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			req.Header.Set("X-Hub-Signature-256", signGitHub("test-secret", body))
			rec := httptest.NewRecorder()
			srv.Handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestWebhook_RejectsOversizeBody(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeEnqueuer{}, "", nil)
	big := bytes.Repeat([]byte("x"), maxWebhookBodyBytes+1)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(big))
	req.Header.Set("X-GitHub-Delivery", "x")
	req.Header.Set("X-GitHub-Event", "pull_request")
	// Signature doesn't matter — size check trips first.
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestWebhook_AllowListIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	enq := &fakeEnqueuer{}
	srv := newTestServer(t, enq, "", nil)
	body := webhookBody("ACME/RePo") // mixed case

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", "x")
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", signGitHub("test-secret", body))

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code)
}

// ---- /api/v1/sprints/{id}/release-notes -------------------------------

func TestSprintsAPI_DisabledWithoutAPIKey(t *testing.T) {
	t.Parallel()
	enq := &fakeEnqueuer{}
	srv := newTestServer(t, enq, "", nil)
	sprintID := uuid.New().String()

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sprints/"+sprintID+"/release-notes", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	// Route not registered → 404 (not 401). This is the "fail
	// closed" behaviour we promise in the operator runbook.
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.False(t, srv.OperatorAPIEnabled())
}

func TestSprintsAPI_RejectsMissingBearer(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeEnqueuer{}, strings.Repeat("k", 32), nil)
	sprintID := uuid.New().String()

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sprints/"+sprintID+"/release-notes", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestSprintsAPI_RejectsWrongBearer(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeEnqueuer{}, strings.Repeat("k", 32), nil)
	sprintID := uuid.New().String()

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sprints/"+sprintID+"/release-notes", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestSprintsAPI_HappyPathWithIdempotencyKey(t *testing.T) {
	t.Parallel()
	enq := &fakeEnqueuer{}
	apiKey := strings.Repeat("k", 32)
	srv := newTestServer(t, enq, apiKey, nil)
	sprintID := uuid.New()

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sprints/"+sprintID.String()+"/release-notes", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Idempotency-Key", "op-1234")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code)
	require.Len(t, enq.releaseNotesCalls, 1)
	got := enq.releaseNotesCalls[0]
	assert.Equal(t, sprintID, got.SprintID)
	assert.Equal(t, "op-1234", got.IdempotencyKey)
	assert.Equal(t, "operator", got.Reason)
}

func TestSprintsAPI_RejectsBadSprintID(t *testing.T) {
	t.Parallel()
	apiKey := strings.Repeat("k", 32)
	srv := newTestServer(t, &fakeEnqueuer{}, apiKey, nil)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sprints/not-a-uuid/release-notes", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSprintsAPI_RejectsBadIdempotencyKey(t *testing.T) {
	t.Parallel()
	apiKey := strings.Repeat("k", 32)
	srv := newTestServer(t, &fakeEnqueuer{}, apiKey, nil)
	sprintID := uuid.New().String()

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sprints/"+sprintID+"/release-notes", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Idempotency-Key", "has\nnewline")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// ---- /healthz + /readyz ----------------------------------------------

func TestHealth_Liveness(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeEnqueuer{}, "", &fakeHealth{})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHealth_ReadinessOK(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeEnqueuer{}, "", &fakeHealth{})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHealth_ReadinessFailsWhenDBDown(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeEnqueuer{}, "",
		&fakeHealth{err: assertErr("db unavailable")})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestHealth_ReadinessFailsWithoutChecker(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeEnqueuer{}, "", nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// ---- middleware: request id ------------------------------------------

func TestRequestID_PropagatesAndEchoes(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeEnqueuer{}, "", &fakeHealth{})

	// (a) header supplied → echoed back.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "given-rid")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, "given-rid", rec.Header().Get("X-Request-ID"))

	// (b) header absent → server mints one.
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	got := rec.Header().Get("X-Request-ID")
	require.NotEmpty(t, got)
	_, err := uuid.Parse(got)
	assert.NoError(t, err, "minted request id should be a UUID")
}

// ---- middleware: panic recovery --------------------------------------

func TestRecover_TurnsPanicInto500Problem(t *testing.T) {
	t.Parallel()
	// Build a server pinned to a handler that panics, going through
	// the same middleware stack as production via chain().
	logger := discardLogger()
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}),
		recoverMiddleware(logger),
		requestIDMiddleware(),
		loggingMiddleware(logger),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
}

// ---- New() validation -------------------------------------------------

func TestNew_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	verifier, _ := infragithub.NewHMACVerifier("s")
	allowed := []apigithub.RepositoryRef{{Owner: "a", Name: "b"}}
	httpCfg := config.HTTP{
		Addr: ":0", ReadTimeout: time.Second, WriteTimeout: time.Second, ShutdownTimeout: time.Second,
	}

	cases := map[string]Deps{
		"no logger":       {Enqueuer: &fakeEnqueuer{}, WebhookVerifier: verifier, AllowedRepos: allowed},
		"no enqueuer":     {Logger: discardLogger(), WebhookVerifier: verifier, AllowedRepos: allowed},
		"no verifier":     {Logger: discardLogger(), Enqueuer: &fakeEnqueuer{}, AllowedRepos: allowed},
		"no allowed list": {Logger: discardLogger(), Enqueuer: &fakeEnqueuer{}, WebhookVerifier: verifier},
	}
	for name, deps := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := New(httpCfg, deps)
			require.Error(t, err)
		})
	}
}

// ---- compile-time guarantee: persistence.Outbox shape unchanged -------

// (Compile-time check on persistence.Outbox lives in
// internal/application/orchestration/enqueuer_test.go via fakeOutbox.)

// assertErr is a tiny sentinel error helper.
type assertErr string

func (a assertErr) Error() string { return string(a) }
