package store

import (
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func parsePoolCfg(t *testing.T) *pgxpool.Config {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://u:p@localhost:5432/db")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return cfg
}

func TestWithMaxConnsSetsPositive(t *testing.T) {
	cfg := parsePoolCfg(t)
	WithMaxConns(20)(cfg)
	if cfg.MaxConns != 20 {
		t.Errorf("MaxConns = %d, want 20", cfg.MaxConns)
	}
}

func TestWithMaxConnsNonPositiveLeavesParsedValue(t *testing.T) {
	for _, n := range []int32{0, -1} {
		cfg := parsePoolCfg(t)
		before := cfg.MaxConns
		WithMaxConns(n)(cfg)
		if cfg.MaxConns != before {
			t.Errorf("WithMaxConns(%d): MaxConns = %d, want unchanged %d", n, cfg.MaxConns, before)
		}
	}
}

// TestWithMaxConnsOverridesCPUDerivedDefault pins that the applied size is independent of the
// host: pgx parses max(4, NumCPU) when no pool_max_conns is given.
func TestWithMaxConnsOverridesCPUDerivedDefault(t *testing.T) {
	cfg := parsePoolCfg(t)
	t.Logf("runtime.NumCPU() = %d, pgx parsed default MaxConns = %d", runtime.NumCPU(), cfg.MaxConns)
	WithMaxConns(20)(cfg)
	if cfg.MaxConns != 20 {
		t.Errorf("MaxConns = %d, want 20 regardless of NumCPU", cfg.MaxConns)
	}
}
