// Command repomind is the entry point and composition root for the
// RepoMind Engineering Intelligence Platform.
//
// All dependency wiring lives here. The remainder of the code base is
// free of construction concerns — Clean Architecture style.
//
// NOTE: this file is intentionally a skeleton in Module 1. Wiring is
// filled in as each subsequent module is delivered.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/repomind/repomind/internal/application/analysis/qa"
	"github.com/repomind/repomind/internal/application/analysis/risk"
	"github.com/repomind/repomind/internal/application/analysis/ticketsummary"
	apigithub "github.com/repomind/repomind/internal/application/github"
	"github.com/repomind/repomind/internal/application/orchestration"
	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/infrastructure/config"
	"github.com/repomind/repomind/internal/infrastructure/confluence"
	infragithub "github.com/repomind/repomind/internal/infrastructure/github"
	infrajira "github.com/repomind/repomind/internal/infrastructure/jira"
	"github.com/repomind/repomind/internal/infrastructure/openai"
	"github.com/repomind/repomind/internal/infrastructure/postgres"
	httpserver "github.com/repomind/repomind/internal/interfaces/http"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "repomind: fatal: %v\n", err)
		os.Exit(1)
	}
}

// run is the actual entry point, separated from main so it can return
// errors and be exercised from tests.
func run(ctx context.Context) error {
	fmt.Printf("repomind %s — starting\n", version)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Structured logger. Driven by REPOMIND_LOG_LEVEL / _FORMAT so
	// dev runs get human-readable text and prod gets JSON. Built
	// here (not in a separate logger package) because main is the
	// only place that needs to make the construction decision.
	logger := newLogger(cfg.Log)
	slog.SetDefault(logger)

	pool, err := postgres.NewPool(ctx, postgres.Config{
		DSN:      cfg.Postgres.DSN,
		MaxConns: cfg.Postgres.MaxConns,
		MinConns: cfg.Postgres.MinConns,
	})
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	fmt.Println("repomind: postgres connection pool ready")

	// Stores + UnitOfWork drive all persistence access. M10's
	// handlers use the UoW to keep "upsert Repo + upsert PR" and
	// "draft Release + save Analysis" atomic.
	stores := postgres.NewStores(pool)
	uow := postgres.NewUnitOfWork(pool)

	// LLM provider (OpenAI-compatible). The same adapter speaks
	// OpenAI, Azure OpenAI, and GitHub Models, selected via
	// REPOMIND_AI_DEPLOYMENT. Not yet consumed — analyzers arrive in
	// Modules 5-7.
	llmProvider, err := openai.NewClient(cfg.OpenAIAdapter())
	if err != nil {
		return fmt.Errorf("openai: %w", err)
	}
	fmt.Printf("repomind: llm provider ready (deployment=%s, model=%s)\n",
		cfg.AI.Deployment, cfg.AI.Model)

	// Risk analyzer (Module 5). The structured-output schema lives
	// with the openai adapter; the analyzer depends only on the
	// vendor-neutral llm.Provider port plus the schema spec.
	riskSchema, ok := openai.SchemaFor(analysis.KindRisk)
	if !ok {
		return fmt.Errorf("openai: missing risk_report schema")
	}
	riskAnalyzer, err := risk.New(risk.Config{
		Provider: llmProvider,
		Schema:   riskSchema,
	})
	if err != nil {
		return fmt.Errorf("risk: %w", err)
	}
	fmt.Println("repomind: risk analyzer ready")

	// QA recommendations analyzer (Module 6). Same wiring shape as the
	// risk analyzer; depends only on the vendor-neutral llm.Provider
	// port plus the qa_recommendations schema.
	qaSchema, ok := openai.SchemaFor(analysis.KindQARecommendations)
	if !ok {
		return fmt.Errorf("openai: missing qa_recommendations schema")
	}
	qaAnalyzer, err := qa.New(qa.Config{
		Provider: llmProvider,
		Schema:   qaSchema,
	})
	if err != nil {
		return fmt.Errorf("qa: %w", err)
	}
	fmt.Println("repomind: qa analyzer ready")

	// Release-notes composer (Module 7) is intentionally NOT wired
	// here: M10's ReleaseNotesHandler renders release notes
	// deterministically per ticket from sprint + PRs (no LLM call).
	// The composer + its embedded schema are kept available in the
	// codebase for future operators who want to opt back into LLM-
	// generated prose by passing Composer in the handler deps; the
	// production wiring just chooses the cheaper, predictable path.
	_ = analysis.KindReleaseNotes
	fmt.Println("repomind: release-notes composer = deterministic per-ticket renderer")

	// Confluence publisher (Module 8). Optional in V1 -- dev runs
	// can skip it and still exercise analyzers end-to-end -- so we
	// only build the adapter when the operator has configured a
	// target. confPublisher is nil when disabled, and the M10
	// release-notes handler is NOT constructed below in that case.
	var (
		confPublisher *confluence.Publisher
		confClient    *confluence.Client
	)
	if cfg.HasConfluence() {
		c, err := confluence.NewClient(cfg.ConfluenceAdapter())
		if err != nil {
			return fmt.Errorf("confluence: %w", err)
		}
		confClient = c
		confPublisher = confluence.NewPublisher(confClient)
		switch {
		case cfg.Confluence.ParentID != "":
			fmt.Printf("repomind: confluence publisher ready (per-sprint mode; space=%s parent=%s)\n",
				cfg.Confluence.SpaceKey, cfg.Confluence.ParentID)
		default:
			fmt.Printf("repomind: confluence publisher ready (single-page mode; page=%s)\n", cfg.Confluence.PageID)
		}
	} else {
		fmt.Println("repomind: confluence publisher disabled (no target configured)")
	}

	// Composition root will assemble (in delivery order):
	//   ✓ 1. Config
	//   ✓ 2. PostgreSQL pool
	//   ✓ 3. Logger / metrics / tracer        (M9: logger only)
	//     4. Jira client            (Module 1)
	//     5. GitHub client          (Module 2)
	//   ✓ 6. OpenAI provider        (Module 4)
	//   ✓ 7a. Risk analyzer         (Module 5)
	//   ✓ 7b. QA analyzer           (Module 6)
	//   ✓ 7c. Release-notes composer (Module 7)
	//   ✓ 8. Confluence publisher   (Module 8)
	//   ✓ 9. HTTP server            (Module 9)
	//    10. Background workers     (Module 10)
	//
	// Each step is added as the corresponding module lands.

	// — Module 9 — HTTP server (webhook ingestion + operator API) —
	//
	// The server's job is narrow: accept inbound deliveries, validate
	// (HMAC for webhooks, bearer for operator), and enqueue an outbox
	// row. The actual work of fetching PRs, running analyzers, and
	// publishing to Confluence lands in M10's dispatcher.
	verifier, err := infragithub.NewHMACVerifier(cfg.GitHub.WebhookSecret)
	if err != nil {
		return fmt.Errorf("github: webhook verifier: %w", err)
	}

	allowed, err := repoAllowList(cfg.GitHub.Repos)
	if err != nil {
		return fmt.Errorf("github: repo allow-list: %w", err)
	}

	// Optional PR target-branch policy (M10). Nil when
	// REPOMIND_GITHUB_REPO_POLICY_PATH is empty -- in that case the
	// webhook + dispatcher accept every branch for every allow-listed
	// repo (legacy behaviour, additive feature).
	branchPolicy, err := cfg.LoadRepoPolicy()
	if err != nil {
		return fmt.Errorf("github: load repo policy: %w", err)
	}
	if branchPolicy != nil {
		logger.Info("repomind: branch policy loaded",
			"path", cfg.GitHub.RepoPolicyPath,
			"default_branches", branchPolicy.DefaultBranches(),
			"override_count", branchPolicy.OverrideCount(),
		)
	} else {
		logger.Info("repomind: no branch policy configured (REPOMIND_GITHUB_REPO_POLICY_PATH empty); all branches accepted")
	}

	enqueuer := orchestration.NewOutboxEnqueuer(stores.Outbox)

	srv, err := httpserver.New(cfg.HTTP, httpserver.Deps{
		Logger:          logger,
		Enqueuer:        enqueuer,
		WebhookVerifier: verifier,
		AllowedRepos:    allowed,
		BranchPolicy:    branchPolicy, // nil = no filtering
		HealthChecker:   pool,
	})
	if err != nil {
		return fmt.Errorf("httpserver: %w", err)
	}
	logger.Info("repomind: http server ready",
		"addr", cfg.HTTP.Addr,
		"operator_api", srv.OperatorAPIEnabled(),
		"allowed_repos", len(allowed),
	)

	// — Module 10 — background workers (dispatcher + ticker) —
	//
	// Construct the App-authed GitHub client + Jira client; build
	// the three event handlers; register them with a Dispatcher;
	// optionally start the SprintTicker. All run as goroutines in
	// the same errgroup as the HTTP server so a single signal
	// (SIGINT/SIGTERM) tears the whole process down cleanly.
	ghAdapter, err := cfg.GitHubAdapter()
	if err != nil {
		return fmt.Errorf("github adapter: %w", err)
	}
	ghClient, err := infragithub.NewClient(ghAdapter)
	if err != nil {
		return fmt.Errorf("github client: %w", err)
	}
	fmt.Println("repomind: github client ready")

	jiraClient, err := infrajira.NewClient(cfg.JiraAdapter())
	if err != nil {
		return fmt.Errorf("jira client: %w", err)
	}
	fmt.Println("repomind: jira client ready")

	// Handler 1/3: github.webhook.received -- route PR events to
	// pr.analysis.requested (no DB writes, no LLM).
	webhookHandler, err := orchestration.NewWebhookHandler(enqueuer, logger, branchPolicy)
	if err != nil {
		return fmt.Errorf("webhook handler: %w", err)
	}

	// Handler 2/3: pr.analysis.requested -- fetch PR, upsert
	// Repo+PR, run risk + QA analyzers, persist Analyses.
	prAnalysisHandler, err := orchestration.NewPRAnalysisHandler(orchestration.PRAnalysisHandlerDeps{
		GitHub:       ghClient,
		Stores:       stores,
		UoW:          uow,
		RiskAnalyser: riskAnalyzer,
		QAAnalyser:   qaAnalyzer,
		Logger:       logger,
	})
	if err != nil {
		return fmt.Errorf("pr-analysis handler: %w", err)
	}

	handlers := orchestration.Handlers{
		string(orchestration.EventGithubWebhookReceived): webhookHandler,
		string(orchestration.EventPRAnalysisRequested):   prAnalysisHandler,
	}

	// Handler 3/3: release_notes.requested -- ONLY registered when
	// Confluence is wired (you can't publish to nowhere). Operator
	// triggers + ticker fires will land in 'no handler' permanent
	// failures otherwise -- which is the correct fail-loud behaviour
	// for a misconfigured deployment.
	if confPublisher != nil {
		// Per-ticket AI summariser (drops a one-sentence Description
		// into each row of the release-log table). Optional -- the
		// handler tolerates nil and renders empty Description cells.
		ticketSummariser, err := ticketsummary.New(ticketsummary.Config{
			Provider: llmProvider,
			Logger:   logger,
		})
		if err != nil {
			return fmt.Errorf("ticketsummary: %w", err)
		}
		fmt.Println("repomind: ticket summariser ready (gpt-4o-mini via RoleQA)")

		// Per-sprint page resolver (M-post-10): when ParentID is set
		// we run in per-sprint mode -- every sprint gets its own
		// child page under the configured Confluence folder/page.
		// Nil in legacy single-page mode, in which case the handler
		// falls back to cfg.Confluence.PageID.
		var sprintPages orchestration.SprintPageIDResolver
		if cfg.Confluence.ParentID != "" {
			r, err := orchestration.NewSprintPageResolver(orchestration.SprintPageResolver{
				Client:        newConfluenceCreatorAdapter(confClient),
				Store:         stores.SprintPages,
				SpaceKey:      cfg.Confluence.SpaceKey,
				ParentID:      cfg.Confluence.ParentID,
				HeadingPrefix: cfg.Jira.TeamName,
			})
			if err != nil {
				return fmt.Errorf("sprint page resolver: %w", err)
			}
			sprintPages = r
			fmt.Printf("repomind: per-sprint page resolver ready (parent=%s space=%s prefix=%q)\n",
				cfg.Confluence.ParentID, cfg.Confluence.SpaceKey, cfg.Jira.TeamName)
		}

		rnHandler, err := orchestration.NewReleaseNotesHandler(orchestration.ReleaseNotesHandlerDeps{
			Jira:   jiraClient,
			Stores: stores,
			UoW:    uow,
			// Composer intentionally left nil: release notes are
			// rendered deterministically per ticket from sprint + PRs
			// (orchestration.RenderPerTicket) -- no LLM call, no
			// hallucinations, byte-identical output across re-runs.
			Summariser:       ticketSummariser,
			Publisher:        confPublisher,
			ConfluencePageID: cfg.Confluence.PageID,
			PerSprintPages:   sprintPages,
			HeadingPrefix:    cfg.Jira.TeamName,
			JiraBaseURL:      cfg.Jira.BaseURL,
			Logger:           logger,
		})
		if err != nil {
			return fmt.Errorf("release-notes handler: %w", err)
		}
		handlers[string(orchestration.EventReleaseNotesRequested)] = rnHandler
	}

	dispatcher, err := orchestration.NewDispatcher(
		orchestration.DispatcherConfig{
			PollInterval:   cfg.Worker.PollInterval,
			BatchSize:      cfg.Worker.BatchSize,
			MaxAttempts:    cfg.Worker.MaxAttempts,
			BaseBackoff:    cfg.Worker.BaseBackoff,
			HandlerTimeout: cfg.Worker.HandlerTimeout,
			ShutdownGrace:  cfg.Worker.ShutdownGrace,
		},
		orchestration.DispatcherDeps{
			Pool:     pool,
			Outbox:   stores.Outbox,
			Handlers: handlers,
			Logger:   logger,
		},
	)
	if err != nil {
		return fmt.Errorf("dispatcher: %w", err)
	}
	logger.Info("repomind: dispatcher ready",
		"registered", dispatcher.RegisteredEventTypes())

	// Sprint ticker (optional, on-by-default). Skip when disabled
	// or when Confluence is absent (no point detecting transitions
	// if we can't publish anywhere). Single-team V1.
	var ticker *orchestration.SprintTicker
	if cfg.Ticker.Enabled && confPublisher != nil {
		tk, err := orchestration.NewSprintTicker(
			orchestration.SprintTickerConfig{
				TeamName:         cfg.Jira.TeamName,
				BoardID:          int64(cfg.Jira.BoardID),
				SprintNamePrefix: cfg.Jira.ResolvedSprintNamePrefix(),
				Interval:         cfg.Ticker.Interval,
			},
			orchestration.SprintTickerDeps{
				Jira:     jiraClient,
				Stores:   stores,
				UoW:      uow,
				Enqueuer: enqueuer,
				Logger:   logger,
			},
		)
		if err != nil {
			return fmt.Errorf("ticker: %w", err)
		}
		ticker = tk
		logger.Info("repomind: sprint ticker ready",
			"team", cfg.Jira.TeamName,
			"interval", cfg.Ticker.Interval)
	} else {
		reason := "ticker disabled via REPOMIND_TICKER_ENABLED=false"
		if confPublisher == nil {
			reason = "ticker skipped (no Confluence target configured)"
		}
		logger.Info("repomind: " + reason)
	}

	// Run HTTP server + dispatcher + (optional) ticker as siblings
	// in one errgroup. ctx cancellation (signal) propagates to all
	// three; the first non-nil error tears down the rest.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(gctx) })
	g.Go(func() error { return dispatcher.Run(gctx) })
	if ticker != nil {
		g.Go(func() error { return ticker.Run(gctx) })
	}

	if err := g.Wait(); err != nil {
		logger.Error("repomind: terminating with error", "err", err)
		return err
	}
	logger.Info("repomind: shutdown complete")
	return nil
}

// newLogger constructs a slog.Logger from config.Log. JSON for prod,
// text for dev; level is parsed leniently (validate() already
// rejected bad values).
func newLogger(c config.Log) *slog.Logger {
	var lvl slog.Level
	switch c.Level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if c.Format == "console" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// repoAllowList parses the CSV from the config into the application-
// port type the HTTP server expects. The infra adapter has its own
// parser but returns `infragithub.RepoSpec` — we shadow that here so
// the interfaces layer never imports an adapter package.
func repoAllowList(csv string) ([]apigithub.RepositoryRef, error) {
	specs, err := infragithub.ParseRepoList(csv)
	if err != nil {
		return nil, err
	}
	out := make([]apigithub.RepositoryRef, 0, len(specs))
	for _, s := range specs {
		out = append(out, apigithub.RepositoryRef{Owner: s.Owner, Name: s.Name})
	}
	return out, nil
}

// confluenceCreatorAdapter bridges the concrete
// *confluence.Client.CreatePage signature to the narrow
// orchestration.ConfluenceCreator interface expected by
// SprintPageResolver -- keeping the application layer free of any
// import on the infra/confluence package.
type confluenceCreatorAdapter struct{ client *confluence.Client }

func newConfluenceCreatorAdapter(c *confluence.Client) *confluenceCreatorAdapter {
	return &confluenceCreatorAdapter{client: c}
}

func (a *confluenceCreatorAdapter) CreatePage(ctx context.Context, in orchestration.ConfluenceCreateInput) (orchestration.ConfluenceCreatedPage, error) {
	pg, err := a.client.CreatePage(ctx, confluence.CreateInput{
		SpaceKey:    in.SpaceKey,
		ParentID:    in.ParentID,
		Title:       in.Title,
		BodyStorage: in.BodyStorage,
	})
	if err != nil {
		return orchestration.ConfluenceCreatedPage{}, err
	}
	return orchestration.ConfluenceCreatedPage{ID: pg.ID, Title: pg.Title}, nil
}
