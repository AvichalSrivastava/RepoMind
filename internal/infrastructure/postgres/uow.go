package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/repomind/repomind/internal/application/persistence"
)

// NewStores returns a persistence.Stores bound to the given pool. Use
// this for read-only queries or single-statement writes that do not
// need transactional grouping. Multi-aggregate writes that must be
// atomic should go through UnitOfWork.Within instead.
func NewStores(pool *pgxpool.Pool) persistence.Stores {
	return buildStores(pool)
}

// NewUnitOfWork returns the canonical UnitOfWork backed by pool. Every
// call to Within opens a fresh pgx transaction, builds tx-bound Stores
// for the callback, commits on success, and rolls back on error or
// panic.
func NewUnitOfWork(pool *pgxpool.Pool) persistence.UnitOfWork {
	return &unitOfWork{pool: pool}
}

type unitOfWork struct {
	pool *pgxpool.Pool
}

func (u *unitOfWork) Within(
	ctx context.Context,
	fn func(ctx context.Context, s persistence.Stores) error,
) error {
	tx, err := u.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return classify("begin tx", err)
	}

	// Defer rollback in case of panic or early return; if commit
	// succeeds, Rollback becomes a no-op returning ErrTxClosed.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	stores := buildStores(tx)
	if err := fn(ctx, stores); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return classify("commit tx", err)
	}
	committed = true
	return nil
}

// buildStores wires every adapter against the same dbtx, so the entire
// Stores bundle shares one connection (pool conn or tx).
func buildStores(q dbtx) persistence.Stores {
	return persistence.Stores{
		Sprints:      &sprintsRepo{q: q},
		Repos:        &reposRepo{q: q},
		PullRequests: &pullRequestsRepo{q: q},
		Releases:     &releasesRepo{q: q},
		Analyses:     &analysesRepo{q: q},
		Outbox:       &outboxRepo{q: q},
		WorkerState:  &workerStateRepo{q: q},
		SprintPages:  &sprintPagesRepo{q: q},
	}
}

// Ensure unitOfWork satisfies the interface at compile time.
var _ persistence.UnitOfWork = (*unitOfWork)(nil)
