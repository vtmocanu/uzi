package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// poolTestDSN carries no password: neither test below opens a connection.
const poolTestDSN = "postgres://u@localhost:5432/db?pool_max_conns=3"

// TestWithMaxConnsSetsMaxConns pins that WithMaxConns overrides the parsed pool size, and
// that a non-positive value leaves the parsed value (here the DSN's pool_max_conns) alone.
func TestWithMaxConnsSetsMaxConns(t *testing.T) {
	for _, tc := range []struct {
		n    int32
		want int32
	}{{7, 7}, {0, 3}, {-1, 3}} {
		cfg, err := pgxpool.ParseConfig(poolTestDSN)
		if err != nil {
			t.Fatalf("ParseConfig: %v", err)
		}
		WithMaxConns(tc.n)(cfg)
		if cfg.MaxConns != tc.want {
			t.Errorf("WithMaxConns(%d): MaxConns = %d, want %d", tc.n, cfg.MaxConns, tc.want)
		}
	}
}

// TestOpenPoolBadDSNFails pins that OpenPool rejects an unparseable DSN value before any
// connection is attempted, wrapping the parse error as "create pool".
func TestOpenPoolBadDSNFails(t *testing.T) {
	pool, err := OpenPool(context.Background(), "postgres://u@localhost:5432/db?statement_cache_capacity=notanumber")
	if err == nil {
		pool.Close()
		t.Fatal("OpenPool accepted statement_cache_capacity=notanumber")
	}
	if !strings.Contains(err.Error(), "create pool") {
		t.Errorf("OpenPool error = %q, want it to contain \"create pool\"", err)
	}
}
