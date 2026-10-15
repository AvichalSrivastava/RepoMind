package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/domain/repository"
	"github.com/repomind/repomind/internal/domain/shared"
)

type reposRepo struct{ q dbtx }

var _ persistence.Repos = (*reposRepo)(nil)

const repoSelectCols = `
	id, owner, name, default_branch, qa_branch, uat_branch, production_branch,
	created_at, last_observed_at
`

func (r *reposRepo) Save(ctx context.Context, rep *repository.Repository) error {
	const q = `
INSERT INTO repository (
	id, owner, name, default_branch, qa_branch, uat_branch, production_branch,
	created_at, last_observed_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (owner, name) DO UPDATE SET
	default_branch    = EXCLUDED.default_branch,
	qa_branch         = EXCLUDED.qa_branch,
	uat_branch        = EXCLUDED.uat_branch,
	production_branch = EXCLUDED.production_branch,
	last_observed_at  = EXCLUDED.last_observed_at`

	var lastObserved *time.Time
	if t := rep.LastObservedAt(); !t.IsZero() {
		v := t.UTC()
		lastObserved = &v
	}
	_, err := r.q.Exec(ctx, q,
		rep.ID().UUID(),
		rep.Owner(),
		rep.Name(),
		rep.DefaultBranch().String(),
		rep.QABranch().String(),
		rep.UATBranch().String(),
		rep.ProductionBranch().String(),
		rep.CreatedAt(),
		lastObserved,
	)
	return classify("save repository", err)
}

func (r *reposRepo) FindByID(ctx context.Context, id shared.RepositoryID) (*repository.Repository, error) {
	return r.findOne(ctx, "WHERE id = $1", id.UUID())
}

func (r *reposRepo) FindByFullName(ctx context.Context, owner, name string) (*repository.Repository, error) {
	return r.findOne(ctx, "WHERE owner = $1 AND name = $2", owner, name)
}

func (r *reposRepo) findOne(ctx context.Context, where string, args ...any) (*repository.Repository, error) {
	row := r.q.QueryRow(ctx, "SELECT "+repoSelectCols+" FROM repository "+where, args...)

	var (
		rawID          uuid.UUID
		owner          string
		name           string
		defBr          string
		qaBr           string
		uatBr          string
		prodBr         string
		createdAt      time.Time
		lastObservedAt *time.Time
	)
	if err := row.Scan(
		&rawID, &owner, &name, &defBr, &qaBr, &uatBr, &prodBr,
		&createdAt, &lastObservedAt,
	); err != nil {
		return nil, classify("find repository", err)
	}
	id, err := shared.ParseRepositoryID(rawID.String())
	if err != nil {
		return nil, classify("find repository: corrupt id", err)
	}
	defB, err1 := shared.NewBranch(defBr)
	qaB, err2 := shared.NewBranch(qaBr)
	uatB, err3 := shared.NewBranch(uatBr)
	prodB, err4 := shared.NewBranch(prodBr)
	if firstErr := errors.Join(err1, err2, err3, err4); firstErr != nil {
		return nil, classify("find repository: corrupt branch refs", firstErr)
	}

	var lo time.Time
	if lastObservedAt != nil {
		lo = lastObservedAt.UTC()
	}
	return repository.Rehydrate(id, owner, name, defB, qaB, uatB, prodB,
		createdAt.UTC(), lo), nil
}
