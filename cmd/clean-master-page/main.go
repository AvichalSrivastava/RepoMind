// Command clean-master-page overwrites the configured Confluence page
// (REPOMIND_CONFLUENCE_PAGE_ID) with a minimal placeholder body: an
// H2 header + an empty release-log table shell carrying the same
// section anchor the publisher expects. The page id and version
// history are preserved so the URL stays stable and Confluence's
// built-in "Page history" can restore any prior version.
//
// Why it exists: after truncating the domain tables, the historical
// rows on the master page no longer correspond to anything in the
// database. Rather than leave stale content, this CLI resets the
// page to a clean canvas that the next publish (or a re-publish
// after backfill) can populate fresh.
//
// Usage:
//
//	docker compose -f deploy/docker-compose.yml \
//	  --env-file .env --env-file .env.secrets \
//	  run --rm --entrypoint /app/clean-master-page repomind \
//	    [-page-id 345833780] [-dry-run]
//
// Exit codes: 0 = success, 1 = fatal (missing config, HTTP failure).
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

	"github.com/repomind/repomind/internal/infrastructure/config"
	"github.com/repomind/repomind/internal/infrastructure/confluence"
)

// placeholderBody is the storage-format XHTML written into the page
// after truncation. It carries:
//   - An H2 header explaining the state.
//   - The same `<!-- repomind:section:start ... -->` anchor the
//     publisher looks for, so a subsequent publish will append rows
//     into the shell instead of creating a duplicate table below.
//   - A table with only the header row and no data rows.
//
// Storage format is intentionally hand-written (not routed through
// the publisher's row-builder) because we have zero rows to render
// and want a deterministic canvas.
const placeholderBody = `<h2>Release Notes Generator</h2>` +
	`<p><em>The database was flushed on ` + `{{.Date}}` + `. This page has been reset; ` +
	`the next release-notes publish will populate rows here.</em></p>` +
	`<!-- repomind:section:start name=release-log-table -->` +
	`<table>` +
	`<colgroup>` +
	`<col style="width: 120px" />` +
	`<col style="width: 110px" />` +
	`<col style="width: 90px" />` +
	`<col style="width: 130px" />` +
	`<col style="width: 320px" />` +
	`<col style="width: 140px" />` +
	`<col style="width: 140px" />` +
	`<col style="width: 220px" />` +
	`<col style="width: 110px" />` +
	`</colgroup>` +
	`<thead><tr>` +
	`<th>Release Planned Date</th>` +
	`<th>Sprint Start</th>` +
	`<th>Sprint ID</th>` +
	`<th>Ticket ID</th>` +
	`<th>Description</th>` +
	`<th>Dev Owner</th>` +
	`<th>QA Owner</th>` +
	`<th>Release Details</th>` +
	`<th>Sprint End</th>` +
	`</tr></thead>` +
	`<tbody></tbody>` +
	`</table>` +
	`<!-- repomind:section:end name=release-log-table -->`

type flags struct {
	pageIDOverride string
	dryRun         bool
}

func parseFlags(args []string) (*flags, error) {
	fs := flag.NewFlagSet("clean-master-page", flag.ContinueOnError)
	f := &flags{}
	fs.StringVar(&f.pageIDOverride, "page-id", "",
		"override REPOMIND_CONFLUENCE_PAGE_ID (numeric Confluence page id)")
	fs.BoolVar(&f.dryRun, "dry-run", false,
		"fetch the page and print current version + body size, but do NOT write")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return f, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "clean-master-page: fatal: %v\n", err)
		os.Exit(1)
	}
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

	pageID := strings.TrimSpace(cfg.Confluence.PageID)
	if v := strings.TrimSpace(f.pageIDOverride); v != "" {
		pageID = v
	}
	if pageID == "" {
		return errors.New("REPOMIND_CONFLUENCE_PAGE_ID is required (or pass -page-id)")
	}

	client, err := confluence.NewClient(cfg.ConfluenceAdapter())
	if err != nil {
		return fmt.Errorf("confluence: %w", err)
	}

	page, err := client.GetPage(ctx, pageID)
	if err != nil {
		return fmt.Errorf("confluence: get page %s: %w", pageID, err)
	}
	logger.Info("fetched current page",
		"page_id", page.ID,
		"title", page.Title,
		"current_version", page.Version,
		"current_body_bytes", len(page.BodyStorage),
	)

	// Fill in the {{.Date}} placeholder without pulling in text/template.
	body := strings.ReplaceAll(placeholderBody, `{{.Date}}`, "2026-07-09")

	if f.dryRun {
		logger.Info("dry-run: would PUT placeholder body; no write performed",
			"new_body_bytes", len(body))
		return nil
	}

	updated, err := client.UpdatePage(ctx, confluence.UpdateInput{
		ID:          page.ID,
		Title:       page.Title,
		NewVersion:  page.Version + 1,
		BodyStorage: body,
	})
	if err != nil {
		return fmt.Errorf("confluence: update page %s: %w", pageID, err)
	}
	logger.Info("page cleaned",
		"page_id", updated.ID,
		"new_version", updated.Version,
		"web_url", updated.WebURL,
	)
	return nil
}
