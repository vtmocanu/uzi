package store

import (
	"strings"
	"testing"
	"time"
)

// poolCfgDSNs are the two DSN syntaxes pgx accepts; every pool-default assertion runs on both.
var poolCfgDSNs = map[string]string{
	"url":     "postgres://u:p@localhost:5432/db",
	"keyword": "host=localhost port=5432 user=u password=p dbname=db",
}

// withParam appends one pool/connection parameter to a DSN in its own syntax.
func withParam(form, dsn, key, value string) string {
	if form == "url" {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + key + "=" + value
	}
	return dsn + " " + key + "=" + value
}

// TestPoolConfigDefaults pins the config OpenPool builds for a DSN that sets none of the
// memory-relevant keys: the statement cache is capped at defaultStatementCacheCapacity
// (ADR-4591), and the lifetime/idle settings keep pgx's own defaults.
func TestPoolConfigDefaults(t *testing.T) {
	for form, dsn := range poolCfgDSNs {
		cfg, err := poolConfig(dsn)
		if err != nil {
			t.Fatalf("%s: poolConfig: %v", form, err)
		}
		if got := cfg.ConnConfig.StatementCacheCapacity; got != defaultStatementCacheCapacity {
			t.Errorf("%s: StatementCacheCapacity = %d, want %d", form, got, defaultStatementCacheCapacity)
		}
		if defaultStatementCacheCapacity != 128 {
			t.Errorf("defaultStatementCacheCapacity = %d, want 128 (the ADR-4591 measured value)", defaultStatementCacheCapacity)
		}
		if got := cfg.ConnConfig.DescriptionCacheCapacity; got != 512 {
			t.Errorf("%s: DescriptionCacheCapacity = %d, want pgx default 512", form, got)
		}
		if got := cfg.MaxConnLifetime; got != time.Hour {
			t.Errorf("%s: MaxConnLifetime = %v, want pgx default 1h", form, got)
		}
		if got := cfg.MaxConnLifetimeJitter; got != 0 {
			t.Errorf("%s: MaxConnLifetimeJitter = %v, want pgx default 0", form, got)
		}
		if got := cfg.MaxConnIdleTime; got != 30*time.Minute {
			t.Errorf("%s: MaxConnIdleTime = %v, want pgx default 30m", form, got)
		}
	}
}

// TestPoolConfigDSNOverridesStatementCache pins that an explicit statement_cache_capacity
// in DATABASE_URL wins over the default, including the values 0 and 512 (pgx's own default,
// which must not be mistaken for "unset").
func TestPoolConfigDSNOverridesStatementCache(t *testing.T) {
	for form, dsn := range poolCfgDSNs {
		for _, tc := range []struct {
			value string
			want  int
		}{{"0", 0}, {"256", 256}, {"512", 512}} {
			cfg, err := poolConfig(withParam(form, dsn, "statement_cache_capacity", tc.value))
			if err != nil {
				t.Fatalf("%s=%s: poolConfig: %v", form, tc.value, err)
			}
			if got := cfg.ConnConfig.StatementCacheCapacity; got != tc.want {
				t.Errorf("%s: statement_cache_capacity=%s gave StatementCacheCapacity = %d, want %d", form, tc.value, got, tc.want)
			}
		}
	}
}

// TestPoolConfigDSNOtherKeysUntouched pins that the other pool/connection keys still flow
// through from DATABASE_URL and do not disturb the statement-cache default.
func TestPoolConfigDSNOtherKeysUntouched(t *testing.T) {
	for form, dsn := range poolCfgDSNs {
		d := withParam(form, dsn, "pool_max_conn_lifetime", "15m")
		d = withParam(form, d, "pool_max_conn_lifetime_jitter", "1m")
		d = withParam(form, d, "pool_max_conn_idle_time", "5m")
		d = withParam(form, d, "description_cache_capacity", "64")
		cfg, err := poolConfig(d)
		if err != nil {
			t.Fatalf("%s: poolConfig: %v", form, err)
		}
		if cfg.MaxConnLifetime != 15*time.Minute || cfg.MaxConnLifetimeJitter != time.Minute || cfg.MaxConnIdleTime != 5*time.Minute {
			t.Errorf("%s: lifetime/jitter/idle = %v/%v/%v, want 15m/1m/5m", form, cfg.MaxConnLifetime, cfg.MaxConnLifetimeJitter, cfg.MaxConnIdleTime)
		}
		if got := cfg.ConnConfig.DescriptionCacheCapacity; got != 64 {
			t.Errorf("%s: DescriptionCacheCapacity = %d, want 64", form, got)
		}
		if got := cfg.ConnConfig.StatementCacheCapacity; got != defaultStatementCacheCapacity {
			t.Errorf("%s: StatementCacheCapacity = %d, want default %d", form, got, defaultStatementCacheCapacity)
		}
	}
}

// TestPoolConfigAppliesOptions pins that PoolOptions still apply after the defaults.
func TestPoolConfigAppliesOptions(t *testing.T) {
	cfg, err := poolConfig(poolCfgDSNs["url"], WithMaxConns(7))
	if err != nil {
		t.Fatalf("poolConfig: %v", err)
	}
	if cfg.MaxConns != 7 {
		t.Errorf("MaxConns = %d, want 7", cfg.MaxConns)
	}
}

// TestPoolConfigBadDSNFails pins that an unparseable DSN still fails rather than getting
// defaults applied to nothing.
func TestPoolConfigBadDSNFails(t *testing.T) {
	if _, err := poolConfig(withParam("url", poolCfgDSNs["url"], "statement_cache_capacity", "notanumber")); err == nil {
		t.Error("poolConfig accepted statement_cache_capacity=notanumber")
	}
}
