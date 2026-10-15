package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	apijira "github.com/repomind/repomind/internal/application/jira"
	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/repomind/repomind/internal/domain/sprint"
)

// SprintTicker is the M10 Phase 6 background goroutine that polls
// Jira for sprint state transitions and enqueues release-notes
// requests. It fires in two situations:
//
//  1. **Active-sprint refresh** — on every tick while a sprint is
//     active, a `release_notes.requested` is enqueued with a
//     tick-bucketed dedup key. This is what makes per-sprint pages
//     appear automatically: the first tick after a sprint becomes
//     active enqueues, the handler runs, the sprint-page resolver
//     creates the page under the configured parent folder, and
//     every subsequent tick keeps it fresh. When nothing has
//     changed since the last publish, the release-notes handler's
//     L3 content-hash short-circuit skips the LLM + Confluence
//     work, so repeated fires are cheap.
//  2. **Active → closed transition** — when the sprint that was
//     active last tick is no longer active AND is now closed, a
//     final publish is enqueued with a close-timestamp dedup key.
//     This is the "seal the release notes" trigger.
//
// V1 scope: ONE team per ticker instance. To support multiple teams,
// the composition root constructs one ticker per team. (Putting a
// fan-out loop inside the ticker is a future-work item, gated by
// when V2 lands which generalises the scalar TEAM/PREFIX/PAGE_ID
// config into a list of triples -- see docs/ARCHITECTURE.md §6.)
//
// Strategy per tick:
//
//  1. Load worker-state bookmark for the team (the JiraSprintID of
//     the active sprint last observed, or 0 on first run).
//  2. List sprints from Jira for the configured board, filtered by
//     SprintNamePrefix (e.g. "Marvelous_Sprint_").
//  3. Mirror each observed sprint to Postgres via Sprints.Save
//     (idempotent on jira_sprint_id). Required so the Phase 5
//     release-notes handler can later FindByID it.
//  4. Identify the current active sprint (if any) and the previously
//     bookmarked one.
//     5a. Closed-transition detect: bookmark != 0 AND that sprint is
//     no longer in 'active' state AND is now closed -> ENQUEUE
//     final release_notes.requested with dedup key
//     "release_notes:sprint=<uuid>:closed=<unix>".
//     5b. Active-sprint refresh: there IS a current active sprint ->
//     ENQUEUE release_notes.requested with dedup key
//     "release_notes:sprint=<uuid>:active_tick=<bucket>" where
//     bucket = now.Truncate(interval).Unix(). One row per tick
//     bucket. Handler's L3 hash short-circuit keeps repeated fires
//     cheap when nothing changed.
//  6. Update bookmark to the current active sprint id (or 0).
//
// Failure handling: any error during a tick is logged and the tick
// is skipped; the bookmark is NOT updated, so the next tick will
// retry. Jira hiccups during a transition might cause us to miss
// the active->closed event for one tick window; on recovery we
// notice the bookmark says X-was-active and Jira says X-is-closed
// and fire the enqueue. So we self-heal across short outages.
type SprintTicker struct {
	cfg  SprintTickerConfig
	deps SprintTickerDeps
}

// SprintTickerConfig holds the team-scoped knobs the ticker needs.
type SprintTickerConfig struct {
	TeamName         string        // matches sprint.Team(); required
	BoardID          int64         // Jira agile board id; required
	SprintNamePrefix string        // e.g. "Marvelous_Sprint_"; required
	Interval         time.Duration // how often to tick; required (> 0)
}

// SprintTickerDeps wires runtime dependencies.
type SprintTickerDeps struct {
	Jira     apijira.Client
	Stores   persistence.Stores
	UoW      persistence.UnitOfWork
	Enqueuer Enqueuer
	Logger   *slog.Logger
	Clock    shared.Clock
}

// tickerBookmark is the JSON shape stored in worker_state.
type tickerBookmark struct {
	// LastActiveJiraSprintID is the JiraSprintID of the sprint that
	// was active when we last ticked. 0 means "no active sprint
	// observed yet" -- first run, or every sprint has been closed
	// and no new one started.
	LastActiveJiraSprintID int64 `json:"last_active_jira_sprint_id"`
	// LastActiveName is captured for human debuggability only.
	LastActiveName string `json:"last_active_name,omitempty"`
	// LastObservedAt anchors "when did we last successfully tick".
	LastObservedAt time.Time `json:"last_observed_at"`
}

// NewSprintTicker validates inputs and constructs a ticker.
func NewSprintTicker(cfg SprintTickerConfig, deps SprintTickerDeps) (*SprintTicker, error) {
	if strings.TrimSpace(cfg.TeamName) == "" {
		return nil, errors.New("orchestration: SprintTicker: TeamName is required")
	}
	if cfg.BoardID <= 0 {
		return nil, errors.New("orchestration: SprintTicker: BoardID must be > 0")
	}
	if strings.TrimSpace(cfg.SprintNamePrefix) == "" {
		return nil, errors.New("orchestration: SprintTicker: SprintNamePrefix is required")
	}
	if cfg.Interval <= 0 {
		return nil, errors.New("orchestration: SprintTicker: Interval must be > 0")
	}
	switch {
	case deps.Jira == nil:
		return nil, errors.New("orchestration: SprintTicker: Jira is required")
	case deps.Stores.Sprints == nil:
		return nil, errors.New("orchestration: SprintTicker: Stores.Sprints is required")
	case deps.Stores.WorkerState == nil:
		return nil, errors.New("orchestration: SprintTicker: Stores.WorkerState is required")
	case deps.UoW == nil:
		return nil, errors.New("orchestration: SprintTicker: UoW is required")
	case deps.Enqueuer == nil:
		return nil, errors.New("orchestration: SprintTicker: Enqueuer is required")
	case deps.Logger == nil:
		return nil, errors.New("orchestration: SprintTicker: Logger is required")
	}
	if deps.Clock == nil {
		deps.Clock = shared.SystemClock{}
	}
	return &SprintTicker{cfg: cfg, deps: deps}, nil
}

// Run loops until ctx is cancelled, calling tickOnce every Interval.
// The first tick happens immediately so a fresh start picks up state
// transitions that happened during downtime.
func (t *SprintTicker) Run(ctx context.Context) error {
	t.deps.Logger.Info("ticker: started",
		"team", t.cfg.TeamName,
		"board_id", t.cfg.BoardID,
		"sprint_prefix", t.cfg.SprintNamePrefix,
		"interval", t.cfg.Interval,
	)
	// Eager first tick.
	t.tickOnce(ctx)

	timer := time.NewTicker(t.cfg.Interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			t.deps.Logger.Info("ticker: ctx cancelled, stopping", "team", t.cfg.TeamName)
			return nil
		case <-timer.C:
			t.tickOnce(ctx)
		}
	}
}

// tickOnce runs one cycle. Never returns an error -- failures are
// logged and absorbed so a transient Jira outage doesn't tear down
// the worker process.
func (t *SprintTicker) tickOnce(ctx context.Context) {
	logger := t.deps.Logger.With("team", t.cfg.TeamName)

	bookmark, err := t.loadBookmark(ctx)
	if err != nil {
		logger.Error("ticker: load bookmark", "err", err)
		return
	}

	// 2. Pull active + closed sprints from Jira for this board.
	jiraSprints, err := t.fetchTeamSprints(ctx)
	if err != nil {
		logger.Error("ticker: fetch sprints", "err", err)
		return
	}
	if len(jiraSprints) == 0 {
		logger.Info("ticker: no sprints found for prefix",
			"prefix", t.cfg.SprintNamePrefix)
		return
	}

	// 3. Mirror each sprint to Postgres so the release-notes handler
	//    can FindByID later.
	for _, js := range jiraSprints {
		if err := t.mirrorSprint(ctx, js); err != nil {
			logger.Error("ticker: mirror sprint",
				"jira_sprint_id", js.ID, "err", err)
			// Don't bail -- mirror failures shouldn't block
			// transition detection for other sprints.
			continue
		}
	}

	// 4. Identify the currently active sprint, if any.
	currentActive := pickActive(jiraSprints)
	var currentActiveID int64
	var currentActiveName string
	if currentActive != nil {
		currentActiveID = currentActive.ID
		currentActiveName = currentActive.Name
	}

	// 5a. Closed-transition detect: the sprint that was active last
	//     tick is no longer active AND is now in the "closed" state.
	//     Fire the final release-notes publish for it with a
	//     transition-bound dedup key so re-firings collapse.
	if bookmark.LastActiveJiraSprintID != 0 &&
		bookmark.LastActiveJiraSprintID != currentActiveID {
		closed := findSprintByID(jiraSprints, bookmark.LastActiveJiraSprintID)
		if closed != nil && strings.EqualFold(closed.State, "closed") {
			if err := t.enqueueClosedReleaseNotes(ctx, closed); err != nil {
				logger.Error("ticker: enqueue closed release notes",
					"jira_sprint_id", closed.ID,
					"name", closed.Name,
					"err", err)
				// Don't update the bookmark on enqueue failure --
				// next tick retries. Outbox dedup collapses duplicates.
				return
			}
			logger.Info("ticker: enqueued closed release notes",
				"jira_sprint_id", closed.ID,
				"name", closed.Name)
		}
	}

	// 5b. Active-sprint refresh: on EVERY tick where a sprint is
	//     active, enqueue a release-notes publish for it. The dedup
	//     key is bucketed by the tick interval so a single row is
	//     kept per bucket even if the ticker retries fast. The
	//     handler's L3 hash short-circuit makes repeated fires cheap
	//     (no LLM cost, no Confluence PUT) when nothing changed.
	//
	//     This is what makes per-sprint page creation "automatic":
	//     the first tick after a sprint becomes active enqueues,
	//     the handler runs, the sprint-page resolver creates the
	//     page under the configured parent folder, and every
	//     subsequent tick keeps it fresh.
	if currentActive != nil {
		bucket := t.deps.Clock.Now().Truncate(t.cfg.Interval).Unix()
		if err := t.enqueueActiveReleaseNotes(ctx, currentActive, bucket); err != nil {
			logger.Error("ticker: enqueue active release notes",
				"jira_sprint_id", currentActive.ID,
				"name", currentActive.Name,
				"err", err)
			// Non-fatal: don't block bookmark update. Next tick
			// gets a fresh bucket and retries automatically.
		} else {
			logger.Info("ticker: enqueued active release notes",
				"jira_sprint_id", currentActive.ID,
				"name", currentActive.Name,
				"tick_bucket", bucket)
		}
	}

	// 6. Update bookmark.
	next := tickerBookmark{
		LastActiveJiraSprintID: currentActiveID,
		LastActiveName:         currentActiveName,
		LastObservedAt:         t.deps.Clock.Now(),
	}
	if err := t.saveBookmark(ctx, next); err != nil {
		logger.Error("ticker: save bookmark", "err", err)
		return
	}
}

// ---------- worker_state helpers ----------

// bookmarkKey returns the well-known worker_state key for this team.
func (t *SprintTicker) bookmarkKey() string {
	return "ticker:team:" + strings.ToLower(t.cfg.TeamName)
}

func (t *SprintTicker) loadBookmark(ctx context.Context) (tickerBookmark, error) {
	raw, err := t.deps.Stores.WorkerState.Get(ctx, t.bookmarkKey())
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return tickerBookmark{}, nil
		}
		return tickerBookmark{}, err
	}
	var bm tickerBookmark
	if err := json.Unmarshal(raw, &bm); err != nil {
		// Corrupt bookmark -- treat as missing and proceed. Logging
		// is the caller's job.
		return tickerBookmark{}, nil
	}
	return bm, nil
}

func (t *SprintTicker) saveBookmark(ctx context.Context, bm tickerBookmark) error {
	raw, err := json.Marshal(bm)
	if err != nil {
		return fmt.Errorf("marshal bookmark: %w", err)
	}
	return t.deps.Stores.WorkerState.Set(ctx, t.bookmarkKey(), raw)
}

// ---------- jira helpers ----------

// fetchTeamSprints returns all sprints on the configured board whose
// names start with SprintNamePrefix, in any state (active + closed +
// future). Paginated under the hood.
func (t *SprintTicker) fetchTeamSprints(ctx context.Context) ([]apijira.Sprint, error) {
	var out []apijira.Sprint
	cursor := ""
	for {
		page, err := t.deps.Jira.ListSprints(ctx, t.cfg.BoardID, apijira.ListSprintsOptions{
			Page:  apijira.PageOptions{Cursor: cursor, Limit: 50},
			State: "active,closed,future",
		})
		if err != nil {
			return nil, err
		}
		if page == nil {
			break
		}
		for _, sp := range page.Items {
			if strings.HasPrefix(strings.ToLower(sp.Name),
				strings.ToLower(t.cfg.SprintNamePrefix)) {
				out = append(out, sp)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// mirrorSprint upserts a Jira sprint into our Sprint aggregate.
// Idempotent via Sprints.Save (ON CONFLICT (jira_sprint_id) DO UPDATE).
func (t *SprintTicker) mirrorSprint(ctx context.Context, js apijira.Sprint) error {
	state, ok := mapSprintState(js.State)
	if !ok {
		// Unknown state -- skip. Future Jira state additions land here.
		return nil
	}
	in := sprint.MirrorInput{
		JiraSprintID: js.ID,
		BoardID:      t.cfg.BoardID,
		Team:         t.cfg.TeamName,
		Name:         js.Name,
		State:        state,
		Goal:         js.Goal,
		StartsAt:     js.StartDate,
		EndsAt:       js.EndDate,
	}
	if state == sprint.StateClosed {
		// MirrorFromJira requires CompletedAt when state=closed.
		// CompleteDate is preferred; fall back to EndDate.
		if js.CompleteDate != nil {
			in.CompletedAt = js.CompleteDate
		} else {
			in.CompletedAt = js.EndDate
		}
	}
	sp, err := sprint.MirrorFromJira(in, t.deps.Clock)
	if err != nil {
		return fmt.Errorf("mirror sprint: %w", err)
	}
	return t.deps.Stores.Sprints.Save(ctx, sp)
}

// enqueueClosedReleaseNotes resolves the closed sprint's local UUID
// and fires a release_notes.requested event with a close-timestamp
// dedup key so retries collapse.
func (t *SprintTicker) enqueueClosedReleaseNotes(ctx context.Context, closed *apijira.Sprint) error {
	sp, err := t.deps.Stores.Sprints.FindByJiraID(ctx, closed.ID)
	if err != nil {
		return fmt.Errorf("find sprint by jira id %d: %w", closed.ID, err)
	}
	// Use the close timestamp (EndDate / CompleteDate) in the dedup
	// key so two distinct close events for the same sprint -- e.g.
	// after a reopen + reclose -- get distinct keys. Falls back to
	// "unknown" when timestamps absent.
	closeStamp := "unknown"
	switch {
	case closed.CompleteDate != nil:
		closeStamp = fmt.Sprintf("%d", closed.CompleteDate.Unix())
	case closed.EndDate != nil:
		closeStamp = fmt.Sprintf("%d", closed.EndDate.Unix())
	}
	_, err = t.deps.Enqueuer.EnqueueReleaseNotesRequest(ctx, ReleaseNotesRequestInput{
		SprintID:       sp.ID().UUID(),
		IdempotencyKey: fmt.Sprintf("release_notes:sprint=%s:closed=%s", sp.ID().String(), closeStamp),
		Reason:         "ticker-closed",
	})
	return err
}

// enqueueActiveReleaseNotes fires a periodic release-notes publish
// for the currently-active sprint. The dedup key includes the tick
// bucket (a monotonic count of tick intervals since the epoch) so
// exactly one outbox row is kept per bucket even if the ticker
// retries rapidly. Cross-bucket firings ARE allowed on purpose --
// they let the handler pick up new PRs / status changes since the
// last publish. When nothing has changed, the L3 hash short-circuit
// in the release-notes handler skips the LLM + Confluence work.
func (t *SprintTicker) enqueueActiveReleaseNotes(ctx context.Context, active *apijira.Sprint, tickBucket int64) error {
	sp, err := t.deps.Stores.Sprints.FindByJiraID(ctx, active.ID)
	if err != nil {
		return fmt.Errorf("find sprint by jira id %d: %w", active.ID, err)
	}
	_, err = t.deps.Enqueuer.EnqueueReleaseNotesRequest(ctx, ReleaseNotesRequestInput{
		SprintID:       sp.ID().UUID(),
		IdempotencyKey: fmt.Sprintf("release_notes:sprint=%s:active_tick=%d", sp.ID().String(), tickBucket),
		Reason:         "ticker-active",
	})
	return err
}

// ---------- pure helpers ----------

// pickActive returns the first sprint with state == "active". Jira
// typically has at most one active per board.
func pickActive(sprints []apijira.Sprint) *apijira.Sprint {
	for i := range sprints {
		if strings.EqualFold(sprints[i].State, "active") {
			return &sprints[i]
		}
	}
	return nil
}

// findSprintByID returns the sprint with the given Jira id, or nil.
func findSprintByID(sprints []apijira.Sprint, id int64) *apijira.Sprint {
	for i := range sprints {
		if sprints[i].ID == id {
			return &sprints[i]
		}
	}
	return nil
}

// mapSprintState maps Jira's string state to the domain enum.
func mapSprintState(s string) (sprint.State, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "future":
		return sprint.StateFuture, true
	case "active":
		return sprint.StateActive, true
	case "closed":
		return sprint.StateClosed, true
	}
	return "", false
}
