// Command backfill-sprint re-ingests every merged pull request in a
// given sprint's date window across the configured allow-listed
// repositories. It bypasses the LLM analysers (risk / QA / ticket
// summariser), so it is safe to run on demand at ~zero cost.
//
// Why it exists: the M10 pipeline populates pull_request.tickets only
// as a side effect of applyGitHubUpdate, which itself only runs when
// a webhook fires. Any PRs merged BEFORE the hotfix that introduced
// ticket extraction (or merged during a webhook outage, or from a
// repo whose GitHub App install was missing) have empty tickets and
// therefore fall out of ListByTickets — leaving the sprint's PR
// column blank on Confluence. Running this CLI once per affected
// sprint restores full coverage.
//
// Usage:
//
//	backfill-sprint \
//	    -sprint 3974           # JiraSprintID; required
//	    [-repos SHL-India/A,SHL-India/B]   # default: all from REPOMIND_GITHUB_REPOS
//	    [-since 2026-06-01T00:00:00Z]      # default: sprint.starts_at
//	    [-until 2026-07-01T00:00:00Z]      # default: sprint.ends_at
//	    [-max-per-repo 500]                # safety cap on PRs scanned per repo
//	    [-dry-run]                         # list matches, don't upsert
//
// Exit codes: 0 = success (may include per-PR errors reported in the
// summary), 1 = fatal (bad config, sprint not found, etc.).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	apigithub "github.com/repomind/repomind/internal/application/github"
	"github.com/repomind/repomind/internal/application/orchestration"
	"github.com/repomind/repomind/internal/infrastructure/config"
	infragithub "github.com/repomind/repomind/internal/infrastructure/github"
	"github.com/repomind/repomind/internal/infrastructure/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "backfill-sprint: fatal: %v\n", err)
		os.Exit(1)
	}
}

type flags struct {
	sprintJiraID int64
	reposCSV     string
	since        string
	until        string
	maxPerRepo   int
	dryRun       bool
}

func parseFlags(args []string) (*flags, error) {
	fs := flag.NewFlagSet("backfill-sprint", flag.ContinueOnError)
	f := &flags{}
	fs.Int64Var(&f.sprintJiraID, "sprint", 0, "Jira sprint id (numeric, e.g. 3974); required")
	fs.StringVar(&f.reposCSV, "repos", "", "comma-separated owner/name list; default = REPOMIND_GITHUB_REPOS")
	fs.StringVar(&f.since, "since", "", "override window start (RFC3339); default = sprint.starts_at")
	fs.StringVar(&f.until, "until", "", "override window end (RFC3339); default = sprint.ends_at")
	fs.IntVar(&f.maxPerRepo, "max-per-repo", 500, "hard cap on PRs scanned per repo (safety)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "list matching PRs without upserting")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if f.sprintJiraID <= 0 {
		return nil, errors.New("-sprint (Jira sprint id) is required")
	}
	if f.maxPerRepo <= 0 {
		return nil, errors.New("-max-per-repo must be > 0")
	}
	return f, nil
}

func run(ctx context.Context) error {
	f, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	pool, err := postgres.NewPool(ctx, postgres.Config{
		DSN:      cfg.Postgres.DSN,
		MaxConns: cfg.Postgres.MaxConns,
		MinConns: cfg.Postgres.MinConns,
	})
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	stores := postgres.NewStores(pool)
	uow := postgres.NewUnitOfWork(pool)

	// Sprint lookup + window resolution.
	sprintAgg, err := stores.Sprints.FindByJiraID(ctx, f.sprintJiraID)
	if err != nil {
		return fmt.Errorf("sprint jira_id=%d: %w", f.sprintJiraID, err)
	}
	window, err := resolveWindow(f, sprintAgg.StartsAt(), sprintAgg.EndsAt())
	if err != nil {
		return fmt.Errorf("window: %w", err)
	}
	logger.Info("sprint resolved",
		"jira_sprint_id", sprintAgg.JiraSprintID(),
		"sprint_name", sprintAgg.Name(),
		"state", string(sprintAgg.State()),
		"window_start", window.start.Format(time.RFC3339),
		"window_end", window.end.Format(time.RFC3339),
	)

	// Repo list: flag override wins; otherwise use configured allow-list.
	reposCSV := f.reposCSV
	if strings.TrimSpace(reposCSV) == "" {
		reposCSV = cfg.GitHub.Repos
	}
	specs, err := infragithub.ParseRepoList(reposCSV)
	if err != nil {
		return fmt.Errorf("repos: %w", err)
	}
	logger.Info("repos to scan", "count", len(specs))

	// GitHub client (App auth) + branch policy.
	ghAdapter, err := cfg.GitHubAdapter()
	if err != nil {
		return fmt.Errorf("github adapter: %w", err)
	}
	ghClient, err := infragithub.NewClient(ghAdapter)
	if err != nil {
		return fmt.Errorf("github client: %w", err)
	}
	branchPolicy, err := cfg.LoadRepoPolicy()
	if err != nil {
		return fmt.Errorf("load repo policy: %w", err)
	}
	if branchPolicy != nil {
		logger.Info("branch policy loaded", "path", cfg.GitHub.RepoPolicyPath)
	} else {
		logger.Info("no branch policy configured; every base branch accepted")
	}

	// Ingestor wires just the "upsert repo + PR" path (no analysers).
	ingestor, err := orchestration.NewPRIngestor(orchestration.PRIngestorDeps{
		GitHub: ghClient,
		Stores: stores,
		UoW:    uow,
		Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("ingestor: %w", err)
	}

	// ------- backfill loop -------
	totalStart := time.Now()
	summary := &runSummary{}
	for _, spec := range specs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ref := orchestration.RepoRef{Owner: spec.Owner, Name: spec.Name}
		rlog := logger.With("repo", spec.String())
		if err := backfillRepo(ctx, rlog, ghClient, branchPolicy, ingestor, ref, window, f, summary); err != nil {
			rlog.Error("repo backfill failed", "err", err)
			summary.repoErrors++
			// Continue to next repo; per-PR failures are tracked separately.
		}
	}

	elapsed := time.Since(totalStart)
	logger.Info("backfill complete",
		"elapsed", elapsed.Round(time.Millisecond).String(),
		"repos_scanned", len(specs),
		"repo_errors", summary.repoErrors,
		"prs_seen", summary.prsSeen,
		"prs_skipped_not_merged", summary.skippedNotMerged,
		"prs_skipped_out_of_window", summary.skippedOutOfWindow,
		"prs_skipped_by_policy", summary.skippedByPolicy,
		"prs_matched", summary.matched,
		"prs_ingested", summary.ingested,
		"prs_failed", summary.failed,
		"dry_run", f.dryRun,
	)
	return nil
}

type window struct {
	start, end time.Time
}

func resolveWindow(f *flags, sprintStart, sprintEnd *time.Time) (window, error) {
	var w window
	if f.since != "" {
		t, err := time.Parse(time.RFC3339, f.since)
		if err != nil {
			return w, fmt.Errorf("-since: %w", err)
		}
		w.start = t
	} else if sprintStart != nil {
		w.start = *sprintStart
	} else {
		return w, errors.New("sprint has no starts_at and no -since override provided")
	}
	if f.until != "" {
		t, err := time.Parse(time.RFC3339, f.until)
		if err != nil {
			return w, fmt.Errorf("-until: %w", err)
		}
		w.end = t
	} else if sprintEnd != nil {
		w.end = *sprintEnd
	} else {
		return w, errors.New("sprint has no ends_at and no -until override provided")
	}
	if !w.start.Before(w.end) {
		return w, fmt.Errorf("window start (%s) must be before end (%s)", w.start, w.end)
	}
	return w, nil
}

type runSummary struct {
	prsSeen            int
	skippedNotMerged   int
	skippedOutOfWindow int
	skippedByPolicy    int
	matched            int
	ingested           int
	failed             int
	repoErrors         int
}

// backfillRepo pages through closed PRs for one repo (most-recent
// first) and ingests those whose merged_at falls inside the window
// AND whose base branch passes the branch policy.
func backfillRepo(
	ctx context.Context,
	logger *slog.Logger,
	gh apigithub.Client,
	policy apigithub.BranchPolicy,
	ingestor *orchestration.PRIngestor,
	ref orchestration.RepoRef,
	win window,
	f *flags,
	summary *runSummary,
) error {
	cursor := ""
	scanned := 0
	// Consecutive-PRs-older-than-window early-break threshold. The
	// GitHub list is updated-desc: once we've seen ~50 PRs in a row
	// whose UpdatedAt precedes the window start, everything older is
	// guaranteed to precede it too (no PR in-window would appear
	// later in the desc-sorted list).
	const oldStreakBreak = 50
	oldStreak := 0

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		page, err := gh.ListPullRequests(ctx, apigithub.RepositoryRef{
			Owner: ref.Owner, Name: ref.Name,
		}, apigithub.ListPROptions{
			State:         apigithub.PRStateClosed, // merged is a subset of closed
			SortDirection: "desc",
			Page:          apigithub.PageOptions{Cursor: cursor, Limit: 100},
		})
		if err != nil {
			return fmt.Errorf("list prs: %w", err)
		}
		if page == nil || len(page.Items) == 0 {
			break
		}

		for _, pr := range page.Items {
			summary.prsSeen++
			scanned++
			if scanned > f.maxPerRepo {
				logger.Warn("hit -max-per-repo cap; stopping this repo",
					"max_per_repo", f.maxPerRepo,
				)
				return nil
			}

			// Merge filter.
			if !pr.Merged || pr.MergedAt == nil {
				summary.skippedNotMerged++
				continue
			}
			// Window filter.
			if pr.MergedAt.Before(win.start) || pr.MergedAt.After(win.end) {
				summary.skippedOutOfWindow++
				// Update-desc early-break tracker.
				if pr.UpdatedAt.Before(win.start) {
					oldStreak++
					if oldStreak >= oldStreakBreak {
						logger.Info("early-break: streak of PRs older than window",
							"streak", oldStreak,
						)
						return nil
					}
				} else {
					oldStreak = 0
				}
				continue
			}
			oldStreak = 0

			// Branch policy filter (nil policy = accept all).
			if policy != nil && !policy.Accept(ref.Owner, ref.Name, pr.BaseBranch) {
				summary.skippedByPolicy++
				logger.Info("skipped by branch policy",
					"pr", pr.Number, "base", pr.BaseBranch, "head", pr.HeadBranch,
					"title", trim(pr.Title, 60),
					"merged_at", pr.MergedAt.Format(time.RFC3339),
				)
				continue
			}

			summary.matched++
			logger.Info("match",
				"pr", pr.Number,
				"base", pr.BaseBranch,
				"head", pr.HeadBranch,
				"title", trim(pr.Title, 80),
				"merged_at", pr.MergedAt.Format(time.RFC3339),
			)

			if f.dryRun {
				continue
			}

			ingested, err := ingestor.UpsertPR(ctx, ref, pr.Number)
			if err != nil {
				summary.failed++
				logger.Error("upsert failed", "pr", pr.Number, "err", err)
				continue
			}
			summary.ingested++
			logger.Info("upserted",
				"pr", pr.Number,
				"pr_id", ingested.ID().String(),
				"ticket_count", len(ingested.Tickets()),
			)
		}

		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
	return nil
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
