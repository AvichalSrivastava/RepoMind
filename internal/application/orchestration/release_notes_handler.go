package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/repomind/repomind/internal/application/analysis/releasenotes"
	"github.com/repomind/repomind/internal/application/analysis/ticketsummary"
	apijira "github.com/repomind/repomind/internal/application/jira"
	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/application/publish"
	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/release"
	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/repomind/repomind/internal/domain/sprint"
)

// ReleaseNotesComposer is the analyser-shaped interface the
// release-notes handler depends on. M7's releasenotes.Analyzer
// satisfies it. Keeping the interface narrow lets us test the handler
// without an LLM provider.
type ReleaseNotesComposer interface {
	Analyze(ctx context.Context, in releasenotes.Input) (*analysis.AnalysisResult, error)
}

// TicketSummariser produces a short one-sentence description for a
// single Jira ticket given its merged PRs. The release-notes handler
// calls it once per ticket-with-PRs and drops the result into the
// Description cell. Optional: when nil, descriptions stay empty for
// the lead to fill manually.
type TicketSummariser interface {
	Summarise(ctx context.Context, in ticketsummary.Input) (string, error)
}

// ReleaseNotesHandlerDeps wires the Phase 5 handler. All fields are
// required except Clock, SkipWindow, Composer, and Summariser
// (defaults / no-ops applied).
type ReleaseNotesHandlerDeps struct {
	Jira             apijira.Client
	Stores           persistence.Stores
	UoW              persistence.UnitOfWork
	Composer         ReleaseNotesComposer // M7 (optional; nil -> deterministic renderer)
	Summariser       TicketSummariser     // optional; nil -> empty Description cells
	Publisher        publish.Publisher    // M8
	ConfluencePageID string               // static page id; used when PerSprintPages is nil (legacy single-page mode)
	PerSprintPages   SprintPageIDResolver // when set, resolves/creates a per-sprint page and overrides ConfluencePageID
	HeadingPrefix    string               // e.g. "Marvelous" -- prepended to sprint name when not already a prefix
	// JiraBaseURL, when set, is used to rewrite every `[KEY](url)`
	// markdown link in the composer output so ticket links point at
	// the real Jira instance instead of the LLM's invented
	// `https://jira.example.com/...` placeholder. Empty disables the
	// rewrite. Recommended value: cfg.Jira.BaseURL.
	JiraBaseURL string
	Logger      *slog.Logger
	Clock       shared.Clock
	// SkipWindow caps how recently a publish must have happened
	// for an identical-content re-trigger to short-circuit. Default
	// 1h. Set to 0 to disable the L3 short-circuit entirely.
	SkipWindow time.Duration
}

// ReleaseNotesHandler composes + publishes release notes for a sprint.
//
// Defense layers enforced (see docs/ARCHITECTURE.md §6):
//   - L1 (caller-side): outbox dedup on event enqueue (M9).
//   - L2 (dispatcher-side): event marked dispatched after success.
//   - L3 (this file): content_hash short-circuit -- if the inputs
//     hash to the same value as the latest published release AND it
//     was published within SkipWindow, skip the LLM and Confluence
//     calls entirely.
//   - L4 (this file): transactional Save of Release + Analysis
//     (UNIQUE(sprint_id, version) prevents duplicate releases).
//   - L5 (M8 Confluence client): full-page PUT replaces content;
//     tickets cannot appear twice in the published page.
//   - L6 (M8 Confluence client + this file): version-optimistic
//     concurrency; Publisher.Publish retries internally on 409
//     up to ConfluenceMaxRetries (configured at composition root).
//
// Pipeline:
//
//  1. Decode payload -> sprint_id.
//  2. Load Sprint from DB. Not found -> permanent failure (operator
//     sent a stale id; no point retrying).
//  3. Fetch the sprint's issues from Jira -> ticket keys.
//  4. Find PRs by ticket. Filter to MERGED only -- release notes
//     are about delivered work.
//  5. Empty PR set -> log + return nil (no work, mark dispatched).
//  6. Compute content_hash = sha256(sprint_id + sorted PR ids + merge SHAs).
//  7. L3 check: latest release for sprint with same hash, published
//     within SkipWindow -> skip.
//  8. Compute next version: v0.0.<count+1>.
//  9. Run M7 composer (LLM call, outside any DB tx).
//  10. UoW: Draft release with hash, include PRs, attach notes, save
//     release + save analysis (audit row).
//  11. Publish to Confluence (full-page PUT, outside tx).
//  12. UoW: release.Publish(now) + save (transitions DB row to
//     published, sets published_at).
//
// Retry semantics: any error before step 10 leaves DB unchanged --
// retry from scratch. After step 10, the draft Release exists with
// content_hash; subsequent retries see it via L3 lookup, find status
// != published, and re-attempt the publish path. Confluence's L5
// full-page PUT makes repeated publishes idempotent.
type ReleaseNotesHandler struct {
	deps ReleaseNotesHandlerDeps
}

// NewReleaseNotesHandler validates deps and constructs the handler.
func NewReleaseNotesHandler(deps ReleaseNotesHandlerDeps) (*ReleaseNotesHandler, error) {
	switch {
	case deps.Jira == nil:
		return nil, errors.New("orchestration: ReleaseNotesHandler: Jira is required")
	case deps.Stores.Sprints == nil:
		return nil, errors.New("orchestration: ReleaseNotesHandler: Stores.Sprints is required")
	case deps.Stores.PullRequests == nil:
		return nil, errors.New("orchestration: ReleaseNotesHandler: Stores.PullRequests is required")
	case deps.Stores.Releases == nil:
		return nil, errors.New("orchestration: ReleaseNotesHandler: Stores.Releases is required")
	case deps.Stores.Analyses == nil:
		return nil, errors.New("orchestration: ReleaseNotesHandler: Stores.Analyses is required")
	case deps.UoW == nil:
		return nil, errors.New("orchestration: ReleaseNotesHandler: UoW is required")
	case deps.Publisher == nil:
		return nil, errors.New("orchestration: ReleaseNotesHandler: Publisher is required")
	case deps.PerSprintPages == nil && strings.TrimSpace(deps.ConfluencePageID) == "":
		return nil, errors.New("orchestration: ReleaseNotesHandler: either ConfluencePageID or PerSprintPages is required")
	case deps.Logger == nil:
		return nil, errors.New("orchestration: ReleaseNotesHandler: Logger is required")
	}
	if deps.Clock == nil {
		deps.Clock = shared.SystemClock{}
	}
	if deps.SkipWindow < 0 {
		deps.SkipWindow = 0
	}
	if deps.SkipWindow == 0 {
		// Caller didn't set it; default to 1h. To disable the L3
		// short-circuit entirely, callers can pass a tiny window
		// (e.g. time.Nanosecond) -- a true zero is treated as
		// "use default".
		deps.SkipWindow = time.Hour
	}
	return &ReleaseNotesHandler{deps: deps}, nil
}

// Compile-time check.
var _ Handler = (*ReleaseNotesHandler)(nil)

// Handle implements the Phase 5 pipeline.
func (h *ReleaseNotesHandler) Handle(ctx context.Context, evt persistence.OutboxEvent) error {
	if evt.EventType != string(EventReleaseNotesRequested) {
		return fmt.Errorf("orchestration: ReleaseNotesHandler: unexpected event_type=%q", evt.EventType)
	}

	var payload ReleaseNotesRequestPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return fmt.Errorf("orchestration: ReleaseNotesHandler: decode payload: %w", err)
	}
	sprintIDRaw := payload.SprintID
	if sprintIDRaw.String() == "00000000-0000-0000-0000-000000000000" {
		return errors.New("orchestration: ReleaseNotesHandler: sprint_id is required")
	}
	sprintID, err := shared.ParseSprintID(sprintIDRaw.String())
	if err != nil {
		return fmt.Errorf("orchestration: ReleaseNotesHandler: parse sprint_id: %w", err)
	}

	logger := h.deps.Logger.With(
		"event_id", evt.ID.String(),
		"sprint_id", sprintID.String(),
		"reason", payload.Reason,
	)

	// Step 2: load sprint from DB.
	sp, err := h.deps.Stores.Sprints.FindByID(ctx, sprintID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			// Not retryable -- the sprint ID is invalid.
			return fmt.Errorf("orchestration: ReleaseNotesHandler: sprint not found: %s", sprintID.String())
		}
		return fmt.Errorf("orchestration: ReleaseNotesHandler: find sprint: %w", err)
	}

	// Step 3: fetch tickets from Jira (keys + full DTOs for owner/summary fallbacks).
	tickets, issuesByKey, err := h.fetchSprintTickets(ctx, sp.JiraSprintID())
	if err != nil {
		return fmt.Errorf("orchestration: ReleaseNotesHandler: fetch tickets: %w", err)
	}
	logger = logger.With("ticket_count", len(tickets))
	if len(tickets) == 0 {
		logger.Info("ReleaseNotesHandler: sprint has no issues, nothing to publish")
		return nil
	}

	// Step 4: find PRs that touched any of those tickets.
	prs, err := h.deps.Stores.PullRequests.ListByTickets(ctx, tickets)
	if err != nil {
		return fmt.Errorf("orchestration: ReleaseNotesHandler: list prs: %w", err)
	}
	merged := filterMerged(prs)
	logger = logger.With("pr_count_total", len(prs), "pr_count_merged", len(merged))

	// Step 6: input hash (order-independent over tickets AND merged PRs,
	// so a re-trigger with the same ticket set + merge SHAs dedups even
	// when zero PRs have landed yet).
	contentHash := computeContentHash(sprintID, tickets, merged)
	logger = logger.With("content_hash", contentHash[:12])

	// Step 7: L3 short-circuit -- only meaningful once at least one
	// release row exists for this sprint. For sprints with zero merged
	// PRs we skip this (no release row could exist yet); the Confluence
	// PUT is idempotent so a re-trigger is a cheap no-op.
	//
	// Operator-forced runs (payload.Force / ?force=true) bypass the
	// short-circuit so a rebuild of dev/QA-owner columns can refresh
	// existing rows even when no inputs changed.
	if len(merged) > 0 && !payload.Force {
		if skip, prior := h.shouldSkip(ctx, sprintID, contentHash); skip {
			logger.Info("ReleaseNotesHandler: content unchanged since last publish, skipping",
				"prior_release_id", prior.ID().String(),
				"prior_published_at", prior.PublishedAt(),
			)
			return nil
		}
	}
	if payload.Force {
		logger.Info("ReleaseNotesHandler: force-refresh requested, bypassing content-hash short-circuit")
	}

	// Build the per-ticket release-log rows for the table publisher.
	// One row per Jira ticket -- rows without a matched merged PR fall
	// back to Jira's summary/assignee so the row still carries useful
	// information (M11).
	logRows, err := h.buildLogRows(ctx, sp, tickets, issuesByKey, merged)
	if err != nil {
		return fmt.Errorf("orchestration: ReleaseNotesHandler: build log rows: %w", err)
	}

	// Also collect PRs merged during the sprint window that couldn't
	// be attributed to any Jira ticket during ingestion. These land
	// in a small "Unlinked PRs" section on the same page so the
	// author can amend the PR title and re-publish -- or an operator
	// can link them manually. The section is authoritative per
	// publish, so fixed PRs drop out on the next run.
	unlinkedPRs, err := h.buildUnlinkedPRRefs(ctx, sp)
	if err != nil {
		// Never fatal -- log and continue with an empty slice so a
		// bad orphan-query doesn't block the whole publish.
		logger.Warn("ReleaseNotesHandler: build unlinked-PRs list failed; continuing without it", "err", err)
		unlinkedPRs = nil
	}
	if len(unlinkedPRs) > 0 {
		logger = logger.With("unlinked_pr_count", len(unlinkedPRs))
	}

	// The release row + audit row track "delivered work". We only
	// persist them when at least one merged PR exists; the domain
	// aggregate requires this invariant. Zero-PR sprints still get a
	// Confluence publish so leads can see every ticket + assign QA.
	var rel *release.Release
	if len(merged) > 0 {
		// Step 8: next version.
		existing, err := h.deps.Stores.Releases.ListBySprint(ctx, sprintID)
		if err != nil {
			return fmt.Errorf("orchestration: ReleaseNotesHandler: list releases: %w", err)
		}
		nextVersion, err := nextReleaseVersion(existing)
		if err != nil {
			return fmt.Errorf("orchestration: ReleaseNotesHandler: next version: %w", err)
		}
		logger = logger.With("new_version", nextVersion.String())

		// Step 9: compose release notes.
		//
		// When Composer is wired we delegate to the M7 LLM composer
		// (legacy behaviour). When Composer is nil we use the
		// deterministic per-ticket renderer instead -- no LLM call, no
		// token cost, byte-identical output across re-runs.
		notes, composed, cerr := h.composeNotes(ctx, sp, tickets, merged)
		if cerr != nil {
			return cerr
		}

		// Step 10: UoW -- save draft release + audit analysis row.
		rel, err = h.saveDraft(ctx, sp, nextVersion, contentHash, merged, notes.Markdown, composed)
		if err != nil {
			return fmt.Errorf("orchestration: ReleaseNotesHandler: save draft: %w", err)
		}
		logger = logger.With("release_id", rel.ID().String())
		logger.Info("ReleaseNotesHandler: draft release saved")
	} else {
		logger.Info("ReleaseNotesHandler: no merged PRs yet -- publishing tickets-only page (table rows will fill in as PRs land)")
	}

	// Step 11: publish to Confluence (outside tx). Table-only output:
	// we hand the publisher per-ticket LogRows but no narrative Notes,
	// so the page accumulates one row per ticket per sprint. Operator
	// hand-edits to any cell are preserved via the per-row anchor,
	// unless payload.Force asks for a full-refresh (backfill path).
	//
	// In per-sprint page mode we first resolve (or create) the target
	// page for this sprint; in legacy single-page mode we reuse the
	// statically-configured ConfluencePageID.
	targetPageID := h.deps.ConfluencePageID
	if h.deps.PerSprintPages != nil {
		resolved, rerr := h.deps.PerSprintPages.ResolveOrCreate(ctx, sp)
		if rerr != nil {
			return fmt.Errorf("orchestration: ReleaseNotesHandler: resolve sprint page: %w", rerr)
		}
		targetPageID = resolved
		logger = logger.With("sprint_page_id", resolved)
	}
	pubResult, err := h.deps.Publisher.Publish(ctx, publish.Input{
		PageID:       targetPageID,
		GeneratedAt:  h.deps.Clock.Now(),
		LogRows:      logRows,
		ForceRefresh: payload.Force,
		UnlinkedPRs:  unlinkedPRs,
	})
	if err != nil {
		return fmt.Errorf("orchestration: ReleaseNotesHandler: publish: %w", err)
	}
	logger.Info("ReleaseNotesHandler: published to confluence",
		"page_url", pubResult.URL, "page_version", pubResult.NewVersion,
		"row_count", len(logRows))

	// Step 12: transition release to published (only when we saved a
	// draft above; zero-PR path has nothing to mark).
	if rel != nil {
		now := h.deps.Clock.Now()
		if err := h.markPublished(ctx, rel, now); err != nil {
			return fmt.Errorf("orchestration: ReleaseNotesHandler: mark published: %w", err)
		}
		logger.Info("ReleaseNotesHandler: release published")
	}
	return nil
}

// fetchSprintTickets pages through Jira's issue list for the sprint
// and collects every issue key as a shared.TicketKey. It also returns
// the full Jira issue DTOs indexed by uppercased ticket key so
// downstream row-building can fall back to the Jira summary /
// assignee when RepoMind has no merged PR yet (M11).
func (h *ReleaseNotesHandler) fetchSprintTickets(
	ctx context.Context,
	jiraSprintID int64,
) ([]shared.TicketKey, map[string]apijira.Issue, error) {
	var out []shared.TicketKey
	issuesByKey := make(map[string]apijira.Issue)
	cursor := ""
	for {
		page, err := h.deps.Jira.ListSprintIssues(ctx, jiraSprintID, apijira.ListSprintIssuesOptions{
			Page: apijira.PageOptions{Cursor: cursor, Limit: 100},
		})
		if err != nil {
			return nil, nil, err
		}
		if page == nil {
			break
		}
		for _, iss := range page.Items {
			tk, err := shared.NewTicketKey(iss.Key)
			if err != nil {
				// Skip malformed keys; don't fail the whole run.
				continue
			}
			out = append(out, tk)
			issuesByKey[strings.ToUpper(iss.Key)] = iss
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, issuesByKey, nil
}

// shouldSkip implements the L3 short-circuit.
func (h *ReleaseNotesHandler) shouldSkip(
	ctx context.Context,
	sprintID shared.SprintID,
	contentHash string,
) (bool, *release.Release) {
	prior, err := h.deps.Stores.Releases.FindLatestBySprint(ctx, sprintID)
	if err != nil || prior == nil {
		return false, nil
	}
	if prior.ContentHash() != contentHash {
		return false, prior
	}
	if !prior.IsPublished() || prior.PublishedAt() == nil {
		// Same hash but never made it to "published" -- complete it.
		return false, prior
	}
	age := h.deps.Clock.Now().Sub(*prior.PublishedAt())
	if age > h.deps.SkipWindow {
		return false, prior
	}
	return true, prior
}

// saveDraft creates the draft Release + persists it alongside an
// audit Analysis row inside a single tx.
func (h *ReleaseNotesHandler) saveDraft(
	ctx context.Context,
	sp *sprint.Sprint,
	version release.Version,
	contentHash string,
	merged []*pullrequest.PullRequest,
	markdown string,
	composed *analysis.AnalysisResult,
) (*release.Release, error) {
	var rel *release.Release
	err := h.deps.UoW.Within(ctx, func(ctx context.Context, s persistence.Stores) error {
		r, err := release.Draft(release.DraftInput{
			SprintID:    sp.ID(),
			Version:     version,
			ContentHash: contentHash,
		}, h.deps.Clock)
		if err != nil {
			return err
		}
		for _, pr := range merged {
			if err := r.IncludePullRequest(pr.ID()); err != nil {
				return fmt.Errorf("include pr: %w", err)
			}
		}
		if err := r.AttachNotes(markdown); err != nil {
			return fmt.Errorf("attach notes: %w", err)
		}
		if err := s.Releases.Save(ctx, r); err != nil {
			return fmt.Errorf("save release: %w", err)
		}
		if err := s.Analyses.Save(ctx, composed); err != nil {
			return fmt.Errorf("save analysis: %w", err)
		}
		rel = r
		return nil
	})
	return rel, err
}

// markPublished transitions the release to published + persists.
func (h *ReleaseNotesHandler) markPublished(ctx context.Context, rel *release.Release, at time.Time) error {
	if err := rel.Publish(at); err != nil {
		return err
	}
	return h.deps.Stores.Releases.Save(ctx, rel)
}

// ---------- helpers ----------

// filterMerged keeps only PRs in the Merged state.
func filterMerged(prs []*pullrequest.PullRequest) []*pullrequest.PullRequest {
	out := make([]*pullrequest.PullRequest, 0, len(prs))
	for _, pr := range prs {
		if pr != nil && pr.IsMerged() {
			out = append(out, pr)
		}
	}
	return out
}

// computeContentHash produces a stable SHA-256 over (sprint_id +
// sorted ticket keys + sorted (pr_id + merge_sha) list). Stable across
// query result orderings -- we sort before hashing. Sensitive to:
// any new PR, any force-push that changed the merge SHA, any PR
// removed from the sprint's tickets, AND any ticket added/removed
// from the sprint (so an empty-PR sprint still gets a fresh hash
// when Jira scope changes).
func computeContentHash(sprintID shared.SprintID, tickets []shared.TicketKey, prs []*pullrequest.PullRequest) string {
	type hashPR struct {
		ID       string
		MergeSHA string
	}
	hp := make([]hashPR, 0, len(prs))
	for _, pr := range prs {
		mergeSHA := ""
		if s := pr.MergeSHA(); s != nil {
			mergeSHA = s.String()
		}
		hp = append(hp, hashPR{ID: pr.ID().String(), MergeSHA: mergeSHA})
	}
	sort.Slice(hp, func(i, j int) bool { return hp[i].ID < hp[j].ID })

	tks := make([]string, 0, len(tickets))
	for _, t := range tickets {
		tks = append(tks, strings.ToUpper(t.String()))
	}
	sort.Strings(tks)

	body := struct {
		SprintID string   `json:"sprint_id"`
		Tickets  []string `json:"tickets"`
		PRs      []hashPR `json:"prs"`
	}{
		SprintID: sprintID.String(),
		Tickets:  tks,
		PRs:      hp,
	}
	raw, _ := json.Marshal(body) // fields are simple, never errors
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// nextReleaseVersion returns the next sequential patch version.
// Existing releases are inspected for their highest patch number;
// the new one is N+1. First release of a sprint gets v0.0.1.
//
// We use v0.0.<n> deliberately -- semantic versioning of releases
// is the operator's call (they may want v1.2.3 for a real product
// release). The auto-numbering is just for in-DB uniqueness.
func nextReleaseVersion(existing []*release.Release) (release.Version, error) {
	highest := 0
	for _, r := range existing {
		s := r.Version().String()
		// Expect "v0.0.<n>" pattern; tolerate anything else by
		// just skipping (caller-managed releases coexist).
		var maj, min, patch int
		if _, err := fmt.Sscanf(s, "v%d.%d.%d", &maj, &min, &patch); err == nil {
			if maj == 0 && min == 0 && patch > highest {
				highest = patch
			}
		}
	}
	return release.NewVersion(fmt.Sprintf("v0.0.%d", highest+1))
}

// extractReleaseNotes type-asserts the composer's output to the
// concrete domain.ReleaseNotes payload.
func extractReleaseNotes(r *analysis.AnalysisResult) (analysis.ReleaseNotes, error) {
	if r == nil {
		return analysis.ReleaseNotes{}, errors.New("analyser returned nil result")
	}
	notes, ok := r.Payload().(analysis.ReleaseNotes)
	if !ok {
		return analysis.ReleaseNotes{}, fmt.Errorf("analyser payload is not ReleaseNotes (got %T)", r.Payload())
	}
	if strings.TrimSpace(notes.Markdown) == "" {
		return analysis.ReleaseNotes{}, errors.New("analyser produced empty markdown")
	}
	return notes, nil
}

// composeNotes returns the release-notes payload to publish plus a
// matching AnalysisResult for the audit row. If the operator wired
// an LLM composer we delegate to it (legacy path); otherwise we
// build the notes deterministically from sprint + tickets + PRs and
// stamp the audit row with a "deterministic" analyzer ref so the
// `analyses` table makes the source obvious.
func (h *ReleaseNotesHandler) composeNotes(
	ctx context.Context,
	sp *sprint.Sprint,
	tickets []shared.TicketKey,
	merged []*pullrequest.PullRequest,
) (analysis.ReleaseNotes, *analysis.AnalysisResult, error) {
	if h.deps.Composer != nil {
		composed, err := h.deps.Composer.Analyze(ctx, releasenotes.Input{
			Sprint: sp, PullRequests: merged,
		})
		if err != nil {
			return analysis.ReleaseNotes{}, nil,
				fmt.Errorf("orchestration: ReleaseNotesHandler: compose: %w", err)
		}
		notes, err := extractReleaseNotes(composed)
		if err != nil {
			return analysis.ReleaseNotes{}, nil,
				fmt.Errorf("orchestration: ReleaseNotesHandler: extract notes: %w", err)
		}
		return notes, composed, nil
	}

	notes, err := RenderPerTicket(PerTicketRenderInput{
		Sprint:      sp,
		Tickets:     tickets,
		PRs:         merged,
		JiraBaseURL: h.deps.JiraBaseURL,
	})
	if err != nil {
		return analysis.ReleaseNotes{}, nil,
			fmt.Errorf("orchestration: ReleaseNotesHandler: deterministic render: %w", err)
	}

	composed, err := analysis.NewResult(
		analysis.KindReleaseNotes,
		analysis.SubjectForSprint(sp.ID()),
		analysis.AnalyzerRef{
			Name:          "release_notes_deterministic",
			Version:       "1.0.0",
			PromptVersion: "n/a",
		},
		notes,
		"", // model: deterministic, no LLM
		0,  // tokensUsed
		h.deps.Clock,
	)
	if err != nil {
		return analysis.ReleaseNotes{}, nil,
			fmt.Errorf("orchestration: ReleaseNotesHandler: build audit result: %w", err)
	}
	return notes, composed, nil
}

// confluenceSectionName produces the marker key the publisher uses
// to locate the right block on the page. Stable across publishes for
// the same sprint -- new sprints get a new section, re-publishes of
// the same sprint replace the existing section in place.
func confluenceSectionName(sp *sprint.Sprint) string {
	return fmt.Sprintf("sprint-%d", sp.JiraSprintID())
}

// confluenceHeading renders the human-readable H2 the publisher
// prepends to the section. Examples:
//
//	"Marvelous Sprint 187 — active"
//	"Marvelous_Sprint_187 — closed 2026-06-28"   (prefix is folded
//	  in because the sprint name already starts with it)
//
// The prefix is skipped when the sprint name already starts with it
// (case-insensitive, optionally followed by '_' or whitespace) so we
// don't render "Marvelous Marvelous_Sprint_187".
func confluenceHeading(prefix string, sp *sprint.Sprint) string {
	name := sp.Name()
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = sp.Team()
	}
	state := string(sp.State())
	header := name
	if !startsWithPrefix(name, prefix) {
		header = prefix + " " + name
	}
	if ends := sp.EndsAt(); ends != nil && state == string(sprint.StateClosed) {
		return fmt.Sprintf("%s \u2014 closed %s", header, ends.UTC().Format("2006-01-02"))
	}
	return fmt.Sprintf("%s \u2014 %s", header, state)
}

// startsWithPrefix reports whether name begins with prefix (case-
// insensitive), allowing an optional separator (' ' or '_') between
// them so both "Marvelous Sprint 187" and "Marvelous_Sprint_187"
// match prefix "Marvelous".
func startsWithPrefix(name, prefix string) bool {
	if prefix == "" {
		return false
	}
	if len(name) < len(prefix) {
		return false
	}
	if !strings.EqualFold(name[:len(prefix)], prefix) {
		return false
	}
	if len(name) == len(prefix) {
		return true
	}
	sep := name[len(prefix)]
	return sep == ' ' || sep == '_'
}

// ticketLinkRE matches a markdown link whose label is a Jira-shaped
// ticket key. We rewrite the URL portion to the configured Jira base
// so the published page never carries the LLM's invented placeholder
// (e.g. https://jira.example.com/browse/SIS-10401).
var ticketLinkRE = regexp.MustCompile(`\[([A-Z][A-Z0-9_]{1,15}-[0-9]+)\]\([^)]*\)`)

// rewriteTicketLinks replaces the URL inside every `[KEY](old)`
// markdown link with `<baseURL>/browse/KEY`. If baseURL is empty it
// returns the input unchanged -- callers that haven't configured a
// Jira base URL keep the LLM's output verbatim.
func rewriteTicketLinks(md, baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return md
	}
	return ticketLinkRE.ReplaceAllStringFunc(md, func(match string) string {
		subs := ticketLinkRE.FindStringSubmatch(match)
		if len(subs) < 2 {
			return match
		}
		return fmt.Sprintf("[%s](%s/browse/%s)", subs[1], baseURL, subs[1])
	})
}

// buildLogRows projects sprint + tickets + merged PRs into a slice
// of publish.ReleaseLogRow -- one row per ticket that has at least
// one merged PR. Tickets with no PR are skipped (silent).
//
// Repository owner/name is resolved per-PR via Stores.Repos so the
// Release Details cell can show the operator-recognised short name.
// A PR whose repo lookup fails is dropped silently and logged.
//
// Iteration order follows Jira's `tickets` order so the rendered
// table is byte-identical across re-runs (operator can rely on
// stable ordering when reading diffs).
func (h *ReleaseNotesHandler) buildLogRows(
	ctx context.Context,
	sp *sprint.Sprint,
	tickets []shared.TicketKey,
	issuesByKey map[string]apijira.Issue,
	merged []*pullrequest.PullRequest,
) ([]publish.ReleaseLogRow, error) {
	// Resolve repository full-names once per RepositoryID.
	repoCache := make(map[shared.RepositoryID]string)
	resolveRepo := func(prRepoID shared.RepositoryID) string {
		if full, ok := repoCache[prRepoID]; ok {
			return full
		}
		var full string
		if h.deps.Stores.Repos == nil {
			full = "repo-" + prRepoID.String()[:8]
		} else {
			r, err := h.deps.Stores.Repos.FindByID(ctx, prRepoID)
			if err != nil {
				h.deps.Logger.Warn("ReleaseNotesHandler: skip PR (repo lookup failed)",
					"repo_id", prRepoID.String(), "err", err)
				return ""
			}
			full = r.FullName()
		}
		repoCache[prRepoID] = full
		return full
	}

	// Index PRs by ticket key (uppercased for case-insensitive match).
	byTicket := make(map[string][]*pullrequest.PullRequest, len(tickets))
	for _, pr := range merged {
		for _, tk := range pr.Tickets() {
			key := strings.ToUpper(tk.String())
			byTicket[key] = append(byTicket[key], pr)
		}
	}

	var startsAt, endsAt time.Time
	if s := sp.StartsAt(); s != nil {
		startsAt = *s
	}
	if e := sp.EndsAt(); e != nil {
		endsAt = *e
	}

	// Ticket-first: emit one row per Jira ticket regardless of whether
	// a merged PR was matched. Missing dev owner / description cells
	// fall back to the Jira issue's assignee / summary so the row
	// still carries useful data for leads (M11).
	rows := make([]publish.ReleaseLogRow, 0, len(tickets))
	for _, t := range tickets {
		key := strings.ToUpper(t.String())
		prs := byTicket[key]
		iss, hasIssue := issuesByKey[key]

		prRefs := make([]publish.PRRef, 0, len(prs))
		devSeen := make(map[string]struct{}, len(prs))
		var devs []string
		for _, pr := range prs {
			repoFull := resolveRepo(pr.RepositoryID())
			if repoFull == "" {
				continue
			}
			prRefs = append(prRefs, publish.PRRef{
				RepoFullName: repoFull,
				Number:       int(pr.Number()),
				Title:        pr.Title(),
				URL:          fmt.Sprintf("https://github.com/%s/pull/%d", repoFull, pr.Number()),
			})
			if a := strings.TrimSpace(pr.Author().Login); a != "" {
				if _, dup := devSeen[a]; !dup {
					devSeen[a] = struct{}{}
					devs = append(devs, a)
				}
			}
		}

		// QA Owner: canonical source is the Jira "QA Owner" custom
		// field (Jira.QAOwnerFieldID). When that field is unset on
		// the issue we fall back to the ticket Reporter -- reporters
		// are frequently the QA lead or the product owner who filed
		// the ticket, and they are NEVER the (post-flip) current
		// assignee, so this fallback is safe against the workflow
		// flip that makes iss.Assignee unreliable for dev/QA
		// attribution.
		var qaOwners []string
		var qaName string
		if hasIssue {
			if q := strings.TrimSpace(iss.QAOwner); q != "" {
				qaName = q
				qaOwners = []string{q}
			} else if r := strings.TrimSpace(iss.Reporter); r != "" {
				qaName = r
				qaOwners = []string{r}
			}
		}

		// Dev owner fallback when there are no PR-derived logins.
		// This project's workflow flips the current Jira assignee to
		// the QA owner after dev-complete, so `iss.Assignee` cannot
		// be trusted as the dev. Priority order:
		//   1. Jira changelog: author of the LAST status transition
		//      into an "In Development"-like status. This is the
		//      strongest signal -- the person who moved the ticket
		//      into dev IS the dev (independent of any later
		//      assignee flip or comment thread noise).
		//   2. Earliest comment author who is neither the QA Owner
		//      nor the current (post-flip) assignee. Tickets that
		//      never had a status transition captured (e.g. old
		//      tickets where changelog was truncated) fall through
		//      to this heuristic.
		//   3. Current Jira assignee, but only when it is not equal
		//      to the QA Owner (workflow-flip hasn't happened yet).
		if len(devs) == 0 && hasIssue {
			if d := pickDevFromTransitions(iss); d != "" {
				devs = []string{d}
			} else if d := pickDevFromComments(iss, qaName); d != "" {
				devs = []string{d}
			} else if a := strings.TrimSpace(iss.Assignee); a != "" &&
				!strings.EqualFold(a, qaName) {
				devs = []string{a}
			}
		}

		// Description: try the LLM summariser when we have PRs, then
		// fall back to the Jira issue summary. Summariser failures MUST
		// NOT fail the publish; we log and continue with the fallback.
		desc := ""
		if h.deps.Summariser != nil && len(prs) > 0 {
			if err := ctx.Err(); err != nil {
				h.deps.Logger.Warn("ReleaseNotesHandler: ctx cancelled mid-publish, skipping remaining summaries",
					"err", err, "ticket", t.String())
			} else {
				s, sErr := h.deps.Summariser.Summarise(ctx, ticketsummary.Input{
					TicketKey: t.String(),
					PRs:       prs,
				})
				if sErr != nil {
					h.deps.Logger.Warn("ReleaseNotesHandler: ticket summary failed",
						"ticket", t.String(), "err", sErr)
				} else {
					desc = s
				}
			}
		}
		if desc == "" && hasIssue {
			desc = strings.TrimSpace(iss.Summary)
		}

		rows = append(rows, publish.ReleaseLogRow{
			SprintID:        sp.ID().UUID(),
			SprintJiraID:    sp.JiraSprintID(),
			SprintNumber:    sprintNumberFromName(sp.Name()),
			SprintStartDate: startsAt,
			SprintEndDate:   endsAt,
			// ReleaseDate intentionally left zero -- the Release
			// Planned Date column stays empty for leads to fill in
			// Confluence; their edit is preserved by the per-row
			// (sprint, ticket) anchor across re-publishes.
			TicketKey:   t.String(),
			Description: desc,
			JiraBaseURL: h.deps.JiraBaseURL,
			DevOwners:   devs,
			QAOwners:    qaOwners,
			PRs:         prRefs,
		})
	}
	return rows, nil
}

// buildUnlinkedPRRefs projects merged PRs that couldn't be attributed
// to any Jira ticket into publish.UnlinkedPRRef values for the
// "Unlinked PRs" section. Scope: PRs whose merged_at falls in the
// sprint's [starts_at, ends_at] window AND whose tickets[] array is
// empty. Returns nil when the sprint has no bounds (unbounded sprint)
// or when Stores.Repos is unavailable (dev/test wiring without repos).
func (h *ReleaseNotesHandler) buildUnlinkedPRRefs(
	ctx context.Context,
	sp *sprint.Sprint,
) ([]publish.UnlinkedPRRef, error) {
	startsPtr, endsPtr := sp.StartsAt(), sp.EndsAt()
	if startsPtr == nil || endsPtr == nil {
		// Unbounded sprint -- we cannot scope orphans by merge date.
		// Return nil (no section rendered) rather than scanning the
		// whole PR table.
		return nil, nil
	}
	orphans, err := h.deps.Stores.PullRequests.ListMergedWithoutTicketsInWindow(ctx, *startsPtr, *endsPtr)
	if err != nil {
		return nil, err
	}
	if len(orphans) == 0 {
		return nil, nil
	}

	repoCache := make(map[shared.RepositoryID]string)
	resolveRepo := func(prRepoID shared.RepositoryID) string {
		if full, ok := repoCache[prRepoID]; ok {
			return full
		}
		var full string
		if h.deps.Stores.Repos == nil {
			full = "repo-" + prRepoID.String()[:8]
		} else {
			r, err := h.deps.Stores.Repos.FindByID(ctx, prRepoID)
			if err != nil {
				h.deps.Logger.Warn("ReleaseNotesHandler: skip unlinked PR (repo lookup failed)",
					"repo_id", prRepoID.String(), "err", err)
				return ""
			}
			full = r.FullName()
		}
		repoCache[prRepoID] = full
		return full
	}

	out := make([]publish.UnlinkedPRRef, 0, len(orphans))
	for _, pr := range orphans {
		repoFull := resolveRepo(pr.RepositoryID())
		if repoFull == "" {
			continue
		}
		mergedAt := time.Time{}
		if m := pr.MergedAt(); m != nil {
			mergedAt = *m
		}
		out = append(out, publish.UnlinkedPRRef{
			RepoFullName: repoFull,
			Number:       int(pr.Number()),
			Title:        pr.Title(),
			Author:       pr.Author().Login,
			HeadBranch:   pr.SourceBranch().String(),
			MergedAt:     mergedAt,
			URL:          fmt.Sprintf("https://github.com/%s/pull/%d", repoFull, pr.Number()),
		})
	}
	return out, nil
}

// pickDevFromComments returns the display name of the earliest
// commenter on the issue whose name is not the QA Owner and not the
// current Jira Assignee (which, in this project's workflow, has
// already been flipped to the QA person after dev-complete). Returns
// "" when no comment qualifies.
//
// The comparison is case-insensitive on trimmed names; empty names
// are always skipped. Comments in iss.Comments are expected to be
// sorted ascending by CreatedAt (the Jira adapter guarantees this).
func pickDevFromComments(iss apijira.Issue, qaName string) string {
	qa := strings.TrimSpace(qaName)
	assignee := strings.TrimSpace(iss.Assignee)
	for _, c := range iss.Comments {
		name := strings.TrimSpace(c.Author)
		if name == "" {
			continue
		}
		if qa != "" && strings.EqualFold(name, qa) {
			continue
		}
		if assignee != "" && strings.EqualFold(name, assignee) {
			continue
		}
		return name
	}
	return ""
}

// pickDevFromTransitions returns the display name of the author of
// the LAST status transition into an "In Development"-like status
// (case-insensitive substring match on "development" against the
// destination status name). Returns "" when the issue has no such
// transition (e.g. changelog was not expanded, or the ticket
// skipped the dev status entirely).
//
// Transitions in iss.StatusTransitions are expected to be sorted
// ascending by CreatedAt (the Jira adapter guarantees this), so we
// scan from the end to pick the most recent qualifying transition.
// This handles tickets that were moved into dev, back out, and into
// dev again -- the last mover is the current dev owner.
func pickDevFromTransitions(iss apijira.Issue) string {
	for i := len(iss.StatusTransitions) - 1; i >= 0; i-- {
		t := iss.StatusTransitions[i]
		if !strings.Contains(strings.ToLower(t.To), "development") {
			continue
		}
		if name := strings.TrimSpace(t.Author); name != "" {
			return name
		}
	}
	return ""
}

// sprintNumberRE captures the trailing integer in a sprint name like
// "Marvelous_Sprint_185" -> "185". Returns 0 when the name does not
// end in digits, which makes the caller fall back to SprintJiraID.
var sprintNumberRE = regexp.MustCompile(`(\d+)\s*$`)

func sprintNumberFromName(name string) int {
	m := sprintNumberRE.FindStringSubmatch(name)
	if len(m) < 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}
