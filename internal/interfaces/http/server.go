package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	apigithub "github.com/repomind/repomind/internal/application/github"
	"github.com/repomind/repomind/internal/application/orchestration"
	"github.com/repomind/repomind/internal/infrastructure/config"
)

// Deps is the dependency bundle the server needs at construction
// time. Composition root populates it; nil fields cause New to fail.
//
// AllowedRepos is the parsed allow-list (operator-supplied via the
// REPOMIND_GITHUB_REPOS env var). The webhook handler enforces it on
// every inbound delivery.
//
// BranchPolicy is optional: when non-nil it filters PR webhooks by
// target branch (see internal/infrastructure/github/policy.go).
// When nil, the legacy behaviour applies (no branch filtering).
//
// HealthChecker is optional but recommended; without it /readyz
// returns 503.
type Deps struct {
	Logger          *slog.Logger
	Enqueuer        orchestration.Enqueuer
	WebhookVerifier apigithub.WebhookVerifier
	AllowedRepos    []apigithub.RepositoryRef
	BranchPolicy    apigithub.BranchPolicy
	HealthChecker   HealthChecker
}

// Server wraps *http.Server with lifecycle helpers and the operator
// flags computed at construction time (e.g. whether the operator API
// was registered).
type Server struct {
	*http.Server
	logger          *slog.Logger
	shutdownTimeout time.Duration
	operatorEnabled bool
}

// OperatorAPIEnabled reports whether the operator API surface was
// registered. Useful for the composition root to log a startup
// breadcrumb so operators don't wonder why their POST 404s.
func (s *Server) OperatorAPIEnabled() bool { return s.operatorEnabled }

// New constructs a fully wired server. It validates Deps and returns
// an error rather than panicking on missing pieces.
//
// Routes registered:
//
//	POST /webhooks/github                          — public (HMAC-verified)
//	POST /api/v1/sprints/{id}/release-notes        — operator (bearer auth)
//	GET  /healthz                                  — liveness
//	GET  /readyz                                   — readiness
//
// The operator surface is registered only when cfg.APIKey is non-empty.
// Otherwise the binary fails *closed* — privileged routes never exist.
func New(cfg config.HTTP, deps Deps) (*Server, error) {
	if deps.Logger == nil {
		return nil, errors.New("httpserver: Logger is required")
	}
	if deps.Enqueuer == nil {
		return nil, errors.New("httpserver: Enqueuer is required")
	}
	if deps.WebhookVerifier == nil {
		return nil, errors.New("httpserver: WebhookVerifier is required")
	}
	if len(deps.AllowedRepos) == 0 {
		return nil, errors.New("httpserver: AllowedRepos must be non-empty (fails closed)")
	}

	mux := http.NewServeMux()

	// — webhook (HMAC-protected by the handler itself) —
	wh := &webhookHandler{
		verifier:     deps.WebhookVerifier,
		enqueuer:     deps.Enqueuer,
		allowedRepos: allowedRepoSet(deps.AllowedRepos),
		branchPolicy: deps.BranchPolicy,
		logger:       deps.Logger,
	}
	mux.Handle("POST /webhooks/github", wh)

	// — health (no auth) —
	hh := &healthHandler{db: deps.HealthChecker}
	mux.HandleFunc("GET /healthz", hh.liveness)
	mux.HandleFunc("GET /readyz", hh.readiness)

	// — operator API (bearer-protected) —
	operatorEnabled := false
	if strings.TrimSpace(cfg.APIKey) != "" {
		operatorEnabled = true
		sh := &sprintsHandler{enqueuer: deps.Enqueuer}
		authed := chain(http.HandlerFunc(sh.releaseNotes),
			bearerAuthMiddleware(cfg.APIKey))
		mux.Handle("POST /api/v1/sprints/{id}/release-notes", authed)
		deps.Logger.Info("httpserver: operator API enabled")
	} else {
		deps.Logger.Warn("httpserver: operator API disabled (REPOMIND_HTTP_API_KEY unset)")
	}

	// Global middleware chain (outermost first).
	root := chain(mux,
		recoverMiddleware(deps.Logger),
		requestIDMiddleware(),
		loggingMiddleware(deps.Logger),
	)

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: root,
		// Header timeout protects against slowloris: the entire
		// request line + headers must arrive within ReadTimeout.
		ReadHeaderTimeout: cfg.ReadTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		// IdleTimeout caps keep-alive connections sitting on the
		// pool. 60s is a conservative ingress-friendly default.
		IdleTimeout: 60 * time.Second,
		// BaseContext: every request inherits a context already
		// tied to the server lifecycle. Shutdown cancels it.
		BaseContext: func(_ net.Listener) context.Context { return context.Background() },
		ErrorLog:    slog.NewLogLogger(deps.Logger.Handler(), slog.LevelError),
	}

	return &Server{
		Server:          srv,
		logger:          deps.Logger,
		shutdownTimeout: cfg.ShutdownTimeout,
		operatorEnabled: operatorEnabled,
	}, nil
}

// Run starts the server and blocks until ctx is cancelled or the
// underlying listener errors. On ctx cancellation it triggers
// graceful Shutdown with the configured timeout.
//
// Lifecycle:
//
//	caller cancels ctx
//	  → Shutdown(deadline = now + shutdownTimeout)
//	  → in-flight requests get up to `deadline` to finish
//	  → ListenAndServe returns http.ErrServerClosed (treated as nil)
//
// Returns the first non-nil of (ListenAndServe error, Shutdown error).
func (s *Server) Run(ctx context.Context) error {
	listenErr := make(chan error, 1)
	go func() {
		s.logger.Info("httpserver: listening", "addr", s.Addr)
		err := s.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
			return
		}
		listenErr <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), s.shutdownTimeout)
		defer cancel()
		s.logger.Info("httpserver: shutting down",
			"timeout", s.shutdownTimeout)
		if err := s.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("httpserver: shutdown: %w", err)
		}
		// Wait for ListenAndServe to return after Shutdown.
		if err := <-listenErr; err != nil {
			return err
		}
		return nil
	case err := <-listenErr:
		return err
	}
}
