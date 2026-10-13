package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolOption adjusts the parsed pool config before the pool is created.
type PoolOption func(*pgxpool.Config)

// WithQueryTracer installs t as the pgx query tracer on every pooled connection.
func WithQueryTracer(t pgx.QueryTracer) PoolOption {
	return func(cfg *pgxpool.Config) { cfg.ConnConfig.Tracer = t }
}

// OpenPool creates a pgx connection pool and verifies connectivity.
func OpenPool(ctx context.Context, dsn string, opts ...PoolOption) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	for _, opt := range opts {
		opt(cfg)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}
