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

// WithMaxConns sets the pool's maximum connection count to n. A non-positive n leaves the
// parsed value (the DSN's pool_max_conns, else pgx's max(4, NumCPU) default) unchanged.
func WithMaxConns(n int32) PoolOption {
	return func(cfg *pgxpool.Config) {
		if n > 0 {
			cfg.MaxConns = n
		}
	}
}

// poolConfig parses dsn into the pool config OpenPool uses, then applies opts. It opens no
// connection, so the resulting config is unit-testable without a database.
func poolConfig(dsn string, opts ...PoolOption) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	for _, opt := range opts {
		opt(cfg)
	}
	return cfg, nil
}

// OpenPool creates a pgx connection pool and verifies connectivity.
func OpenPool(ctx context.Context, dsn string, opts ...PoolOption) (*pgxpool.Pool, error) {
	cfg, err := poolConfig(dsn, opts...)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
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
