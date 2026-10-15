//go:build integration

// Package postgres_test exercises the persistence adapters against a
// real Postgres instance via testcontainers-go. The `integration`
// build tag keeps these tests out of the default `go test ./...` run —
// they require Docker on the host.
//
// Run with:
//
//	go test -tags=integration ./internal/infrastructure/postgres/...
package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/repository"
	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/repomind/repomind/internal/domain/sprint"
	"github.com/repomind/repomind/internal/infrastructure/postgres"
)

// ---------- fixtures / helpers ----------

type fixture struct {
	pool   *pgxpool.Pool
	stores persistence.Stores
	uow    persistence.UnitOfWork
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	pgC, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("repomind"),
		tcpostgres.WithUsername("repomind"),
		tcpostgres.WithPassword("repomind"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(context.Background()) })

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := postgres.NewPool(ctx, postgres.Config{
		DSN:      dsn,
		MaxConns: 5,
		MinConns: 1,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Apply migrations 000001 + 000002 + 000003 + 000004 + 000005 + 000006 in order.
	migDir := repoRoot(t)
	for _, name := range []string{
		"000001_init.up.sql",
		"000002_persistence.up.sql",
		"000003_dispatch.up.sql",
		"000004_outbox_notify_trigger.up.sql",
		"000005_worker_state.up.sql",
		"000006_release_content_hash.up.sql",
	} {
		path := filepath.Join(migDir, "migrations", name)
		sql, err := os.ReadFile(path)
		require.NoErrorf(t, err, "read migration %s", name)
		_, err = pool.Exec(ctx, string(sql))
		require.NoErrorf(t, err, "apply migration %s", name)
	}

	return &fixture{
		pool:   pool,
		stores: postgres.NewStores(pool),
		uow:    postgres.NewUnitOfWork(pool),
	}
}

// repoRoot walks up from the test working dir until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (go.mod)")
		}
		dir = parent
	}
}

func clk() shared.Clock { return shared.SystemClock{} }

func newSprint(t *testing.T, jiraID int64, name string) *sprint.Sprint {
	t.Helper()
	starts := time.Now().Add(-7 * 24 * time.Hour)
	ends := time.Now().Add(7 * 24 * time.Hour)
	s, err := sprint.MirrorFromJira(sprint.MirrorInput{
		JiraSprintID: jiraID,
		BoardID:      1,
		Team:         "MARVELS",
		Name:         name,
		State:        sprint.StateActive,
		Goal:         "ship something",
		StartsAt:     &starts,
		EndsAt:       &ends,
	}, clk())
	require.NoError(t, err)
	return s
}

func newRepo(t *testing.T, owner, name string) *repository.Repository {
	t.Helper()
	r, err := repository.Register(repository.RegistrationInput{
		Owner: owner,
		Name:  name,
	}, clk())
	require.NoError(t, err)
	return r
}

func newPR(t *testing.T, repoID shared.RepositoryID, num pullrequest.Number) *pullrequest.PullRequest {
	t.Helper()
	author, err := shared.NewAuthor("octocat", "Octo Cat", "octo@example.com")
	require.NoError(t, err)
	src, err := shared.NewBranch("feature/x")
	require.NoError(t, err)
	dst, err := shared.NewBranch("main")
	require.NoError(t, err)
	pr, err := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: repoID,
		Number:       num,
		Title:        "Add feature X",
		Description:  "desc",
		Author:       author,
		SourceBranch: src,
		TargetBranch: dst,
	}, clk())
	require.NoError(t, err)
	return pr
}

// ---------- tests ----------

func TestSprintsRoundTrip(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()

	sp := newSprint(t, 101, "Sprint 1")
	require.NoError(t, fx.stores.Sprints.Save(ctx, sp))

	got, err := fx.stores.Sprints.FindByID(ctx, sp.ID())
	require.NoError(t, err)
	require.Equal(t, sp.ID(), got.ID())
	require.Equal(t, sp.JiraSprintID(), got.JiraSprintID())
	require.Equal(t, sp.Team(), got.Team())

	// Upsert is idempotent.
	require.NoError(t, fx.stores.Sprints.Save(ctx, sp))
	got2, err := fx.stores.Sprints.FindByJiraID(ctx, 101)
	require.NoError(t, err)
	require.Equal(t, sp.ID(), got2.ID())
}

func TestReposRoundTrip(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()

	repo := newRepo(t, "MARVELS", "repomind")
	require.NoError(t, fx.stores.Repos.Save(ctx, repo))

	got, err := fx.stores.Repos.FindByFullName(ctx, "MARVELS", "repomind")
	require.NoError(t, err)
	require.Equal(t, repo.ID(), got.ID())
	require.Equal(t, "repomind", got.Name())
	require.Equal(t, "main", got.DefaultBranch().String())
}

func TestUnitOfWorkRollback(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	sp := newSprint(t, 9001, "UoW rollback sprint")

	sentinel := errors.New("intentional rollback")
	err := fx.uow.Within(ctx, func(ctx context.Context, s persistence.Stores) error {
		if err := s.Sprints.Save(ctx, sp); err != nil {
			return err
		}
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	_, err = fx.stores.Sprints.FindByID(ctx, sp.ID())
	require.ErrorIs(t, err, persistence.ErrNotFound)
}

func TestUnitOfWorkCommit(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	sp := newSprint(t, 9002, "UoW commit sprint")

	err := fx.uow.Within(ctx, func(ctx context.Context, s persistence.Stores) error {
		return s.Sprints.Save(ctx, sp)
	})
	require.NoError(t, err)

	got, err := fx.stores.Sprints.FindByID(ctx, sp.ID())
	require.NoError(t, err)
	require.Equal(t, sp.ID(), got.ID())
}

func TestOutboxLifecycle(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()

	payload, _ := json.Marshal(map[string]any{"foo": "bar"})
	evt := persistence.OutboxEvent{
		AggregateType: "sprint",
		AggregateID:   uuid.New(),
		EventType:     "sprint.closed",
		Payload:       payload,
	}
	require.NoError(t, fx.stores.Outbox.Enqueue(ctx, evt))

	pending, err := fx.stores.Outbox.ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "sprint.closed", pending[0].EventType)
	require.Equal(t, persistence.OutboxPending, pending[0].Status)

	require.NoError(t, fx.stores.Outbox.MarkDispatched(ctx, pending[0].ID, time.Now().UTC()))

	pending2, err := fx.stores.Outbox.ListPending(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, pending2, "dispatched events must not be returned")
}

// TestOutboxEnqueueWithDedup exercises the M9 idempotency path:
//   - first insert with a dedup key creates a row and returns DedupHit=false
//   - second insert with the same key is a no-op and returns the same id
//     with DedupHit=true
//   - empty dedup key is rejected (this is the WithDedup contract)
//   - the plain Enqueue path can run alongside (dedup_key NULL ≠ NULL)
func TestOutboxEnqueueWithDedup(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()

	payload, _ := json.Marshal(map[string]any{"delivery_id": "abc"})
	mk := func(key string) persistence.OutboxEvent {
		return persistence.OutboxEvent{
			AggregateType: "webhook",
			AggregateID:   uuid.New(),
			EventType:     "github.webhook.received",
			Payload:       payload,
			DedupKey:      key,
		}
	}

	// 1. fresh key inserts.
	res1, err := fx.stores.Outbox.EnqueueWithDedup(ctx, mk("delivery-uuid-1"))
	require.NoError(t, err)
	require.False(t, res1.DedupHit)
	require.NotEqual(t, uuid.Nil, res1.ID)

	// 2. same key is a no-op and returns the same id.
	res2, err := fx.stores.Outbox.EnqueueWithDedup(ctx, mk("delivery-uuid-1"))
	require.NoError(t, err)
	require.True(t, res2.DedupHit)
	require.Equal(t, res1.ID, res2.ID)

	// 3. only one row exists.
	pending, err := fx.stores.Outbox.ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "delivery-uuid-1", pending[0].DedupKey)

	// 4. empty key rejected.
	_, err = fx.stores.Outbox.EnqueueWithDedup(ctx, mk(""))
	require.Error(t, err)

	// 5. plain Enqueue still works alongside dedup rows because
	//    Postgres treats NULL ≠ NULL in unique indexes.
	require.NoError(t, fx.stores.Outbox.Enqueue(ctx, persistence.OutboxEvent{
		AggregateType: "sprint", AggregateID: uuid.New(),
		EventType: "sprint.scheduled", Payload: payload,
	}))
	require.NoError(t, fx.stores.Outbox.Enqueue(ctx, persistence.OutboxEvent{
		AggregateType: "sprint", AggregateID: uuid.New(),
		EventType: "sprint.scheduled", Payload: payload,
	}))
	pending, err = fx.stores.Outbox.ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 3, "1 dedup + 2 nil-key rows expected")
}

// TestOutboxNotifyChannelFires verifies that Enqueue triggers a
// NOTIFY on persistence.NotifyChannel — the wakeup mechanism M10's
// dispatcher will LISTEN on. The test opens a dedicated connection,
// LISTENs first, then enqueues from a separate connection so the
// notify actually traverses the pubsub layer.
func TestOutboxNotifyChannelFires(t *testing.T) {
	fx := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Take a dedicated listener connection.
	conn, err := fx.pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()

	_, err = conn.Exec(ctx, `LISTEN `+persistence.NotifyChannel)
	require.NoError(t, err)

	payload, _ := json.Marshal(map[string]any{"x": 1})
	require.NoError(t, fx.stores.Outbox.Enqueue(ctx, persistence.OutboxEvent{
		AggregateType: "sprint",
		AggregateID:   uuid.New(),
		EventType:     "notify.test",
		Payload:       payload,
	}))

	// WaitForNotification blocks until a NOTIFY arrives or ctx
	// is cancelled. A 10s timeout is overkill but cheap.
	n, err := conn.Conn().WaitForNotification(ctx)
	require.NoError(t, err)
	require.Equal(t, persistence.NotifyChannel, n.Channel)
	require.NotEmpty(t, n.Payload, "notification payload should be the outbox id")
	_, perr := uuid.Parse(n.Payload)
	require.NoError(t, perr, "notify payload must be a UUID")
}

func TestAnalysisPolymorphicPayload(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()

	repo := newRepo(t, "MARVELS", "analysis-test")
	require.NoError(t, fx.stores.Repos.Save(ctx, repo))

	pr := newPR(t, repo.ID(), pullrequest.Number(42))
	require.NoError(t, fx.stores.PullRequests.Save(ctx, pr))

	risk, err := analysis.NewRiskReport(
		analysis.RiskHigh,
		[]analysis.RiskFactor{analysis.FactorLargeDiff, analysis.FactorDatabaseMigration},
		"This PR touches the auth subsystem during a freeze window.",
		0.87,
	)
	require.NoError(t, err)

	res, err := analysis.NewResult(
		analysis.KindRisk,
		analysis.SubjectForPR(pr.ID()),
		analysis.AnalyzerRef{Name: "risk-v1", Version: "1.0.0", PromptVersion: "p1"},
		risk,
		"gpt-4o-mini",
		1234,
		clk(),
	)
	require.NoError(t, err)
	require.NoError(t, fx.stores.Analyses.Save(ctx, res))

	got, err := fx.stores.Analyses.FindByID(ctx, res.ID())
	require.NoError(t, err)
	require.Equal(t, analysis.KindRisk, got.Kind())
	gotRisk, ok := got.Payload().(analysis.RiskReport)
	require.True(t, ok, "payload must rehydrate to RiskReport, got %T", got.Payload())
	require.Equal(t, analysis.RiskHigh, gotRisk.Level)
	require.Len(t, gotRisk.Factors, 2)

	list, err := fx.stores.Analyses.ListByPullRequest(ctx, pr.ID(), analysis.KindRisk)
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Empty Kind returns all kinds.
	all, err := fx.stores.Analyses.ListByPullRequest(ctx, pr.ID(), "")
	require.NoError(t, err)
	require.Len(t, all, 1)
}

func TestNotFoundIsErrNotFound(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()

	_, err := fx.stores.Sprints.FindByID(ctx, shared.NewSprintID())
	require.ErrorIs(t, err, persistence.ErrNotFound)
}

// ---------- M10 — worker_state ----------

func TestWorkerStateUpsertRoundTrip(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()

	// Missing key -> ErrNotFound (mirrors every other Find* method).
	_, err := fx.stores.WorkerState.Get(ctx, "ticker:sprint:does-not-exist")
	require.ErrorIs(t, err, persistence.ErrNotFound)

	// First Set is an insert.
	v1 := json.RawMessage(`{"jira_sprint_id":187,"state":"active","seen_at":"2026-06-29T00:00:00Z"}`)
	require.NoError(t, fx.stores.WorkerState.Set(ctx, "ticker:sprint:187", v1))

	got, err := fx.stores.WorkerState.Get(ctx, "ticker:sprint:187")
	require.NoError(t, err)
	require.JSONEq(t, string(v1), string(got))

	// Second Set on the same key is an upsert (not a duplicate-key error).
	v2 := json.RawMessage(`{"jira_sprint_id":187,"state":"closed","seen_at":"2026-06-29T01:00:00Z"}`)
	require.NoError(t, fx.stores.WorkerState.Set(ctx, "ticker:sprint:187", v2))

	got2, err := fx.stores.WorkerState.Get(ctx, "ticker:sprint:187")
	require.NoError(t, err)
	require.JSONEq(t, string(v2), string(got2))

	// Empty key is a programmer error, not an SQL error.
	_, err = fx.stores.WorkerState.Get(ctx, "")
	require.Error(t, err)
	require.Error(t, fx.stores.WorkerState.Set(ctx, "", v1))

	// Nil value coerces to '{}' so the NOT NULL column stays happy.
	require.NoError(t, fx.stores.WorkerState.Set(ctx, "ticker:bare-default", nil))
	got3, err := fx.stores.WorkerState.Get(ctx, "ticker:bare-default")
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(got3))
}

func TestWorkerStateUpdatesUpdatedAt(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()

	key := "ticker:updated-at-test"
	require.NoError(t, fx.stores.WorkerState.Set(ctx, key, json.RawMessage(`{"n":1}`)))

	// Read updated_at directly so we can assert it actually moves on
	// re-Set. The adapter doesn't expose it through the port (by
	// design — only the dashboard needs it), so we query the table.
	var firstAt time.Time
	require.NoError(t, fx.pool.QueryRow(ctx,
		`SELECT updated_at FROM worker_state WHERE key = $1`, key).Scan(&firstAt))

	// Sleep just enough that the now() in the next Set is observably
	// later than the first. Postgres timestamp resolution is microsecond
	// so 5ms is generous.
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, fx.stores.WorkerState.Set(ctx, key, json.RawMessage(`{"n":2}`)))

	var secondAt time.Time
	require.NoError(t, fx.pool.QueryRow(ctx,
		`SELECT updated_at FROM worker_state WHERE key = $1`, key).Scan(&secondAt))

	require.True(t, secondAt.After(firstAt),
		"updated_at must bump on upsert: first=%s second=%s", firstAt, secondAt)
}

func TestWorkerStateWithinTransaction(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()

	// The ticker's invariant is "read old state, write new state,
	// enqueue outbox event — all in one tx so a crash mid-flight
	// rolls back cleanly." Verify UoW.Within exposes WorkerState
	// the same way it exposes every other store.
	err := fx.uow.Within(ctx, func(ctx context.Context, s persistence.Stores) error {
		require.NotNil(t, s.WorkerState, "WorkerState must be available inside Within")
		return s.WorkerState.Set(ctx, "ticker:tx-test", json.RawMessage(`{"committed":true}`))
	})
	require.NoError(t, err)

	got, err := fx.stores.WorkerState.Get(ctx, "ticker:tx-test")
	require.NoError(t, err)
	require.JSONEq(t, `{"committed":true}`, string(got))

	// Rollback path: a returned error must discard the write.
	sentinel := errors.New("intentional rollback")
	err = fx.uow.Within(ctx, func(ctx context.Context, s persistence.Stores) error {
		if err := s.WorkerState.Set(ctx, "ticker:tx-rollback",
			json.RawMessage(`{"x":1}`)); err != nil {
			return err
		}
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	_, err = fx.stores.WorkerState.Get(ctx, "ticker:tx-rollback")
	require.ErrorIs(t, err, persistence.ErrNotFound,
		"rolled-back Set must leave no row")
}
