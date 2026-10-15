// Package postgres is the PostgreSQL implementation of the persistence
// ports defined in internal/application/persistence.
//
// Public surface:
//
//	NewPool(ctx, Config) (*pgxpool.Pool, error)  — connection pool factory
//	NewStores(pool *pgxpool.Pool) persistence.Stores
//	NewUnitOfWork(pool *pgxpool.Pool) persistence.UnitOfWork
//
// All repository structs are private; callers obtain them only through
// the Stores / UnitOfWork facades.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config controls pool construction. Defaults match the conservative
// values in internal/infrastructure/config.Postgres.
type Config struct {
	DSN               string
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration // 0 → pgx default (~1h)
	HealthCheckPeriod time.Duration // 0 → pgx default (~1m)
}

// NewPool builds and verifies a pgxpool.Pool. The pool is pinged once
// before being returned; callers may safely use it immediately or
// defer pool.Close().
func NewPool(ctx context.Context, c Config) (*pgxpool.Pool, error) {
	if c.DSN == "" {
		return nil, fmt.Errorf("postgres: DSN is required")
	}

	pcfg, err := pgxpool.ParseConfig(c.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse DSN: %w", err)
	}

	if c.MaxConns > 0 {
		pcfg.MaxConns = c.MaxConns
	}
	if c.MinConns >= 0 {
		pcfg.MinConns = c.MinConns
	}
	if c.MaxConnLifetime > 0 {
		pcfg.MaxConnLifetime = c.MaxConnLifetime
	}
	if c.HealthCheckPeriod > 0 {
		pcfg.HealthCheckPeriod = c.HealthCheckPeriod
	}

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: new pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}
