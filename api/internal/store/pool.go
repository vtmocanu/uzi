package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// defaultStatementCacheCapacity caps pgx's per-connection cache of server-side prepared
// statements (pgx's own default is 512). Each cached statement holds its plan state in the
// backend; ADR-4591 measured about 154 KiB of backend memory contexts per cached statement,
// and with JIT off a 128 cap cut steady backend heap by 16.7% (68.6 to 57.1 MiB) with flat
// ClaimRun / InsertRunMessage latency. 32 thrashed the hot paths. An explicit
// statement_cache_capacity in DATABASE_URL overrides it.
const defaultStatementCacheCapacity = 128

// poolConfig parses dsn into the pool config OpenPool uses, applies the pool defaults the
// DSN does not set itself, then applies opts. It opens no connection, so the resulting
// config is unit-testable without a database.
func poolConfig(dsn string, opts ...PoolOption) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// pgx deletes statement_cache_capacity from the parsed RuntimeParams once it has read it,
	// and 512 is indistinguishable from its default, so whether the DSN set the key is read
	// from a separate pgconn parse (URL, keyword/value and service-file forms alike), as
	// config.resolveDBMaxConns does for pool_max_conns.
	if pc, perr := pgconn.ParseConfig(dsn); perr == nil {
		if _, set := pc.RuntimeParams["statement_cache_capacity"]; !set {
			cfg.ConnConfig.StatementCacheCapacity = defaultStatementCacheCapacity
		}
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
