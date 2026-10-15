package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// dbtx is the minimal pgx surface that both *pgxpool.Pool and pgx.Tx
// satisfy. Every repository accepts a dbtx so the same struct works in
// pool-bound (read) and tx-bound (write) modes.
//
// We intentionally keep this interface small — only methods we use.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
