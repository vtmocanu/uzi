package config

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

const (
	dsnURL5     = "postgres://uzi@db:5432/uzi?sslmode=disable&pool_max_conns=5"
	dsnKV5      = "host=db user=uzi password=pw dbname=uzi sslmode=disable pool_max_conns=5"
	dsnPlain    = "postgres://uzi@db:5432/uzi?sslmode=disable"
	dsnBadParam = "postgres://uzi@db:5432/uzi?sslmode=disable&pool_max_conns=zero"
)

type dbMaxConnsCase struct {
	name       string
	env        string
	dsn        string
	want       int32
	wantSource string
	wantWarn   int
}

func dbMaxConnsCases() []dbMaxConnsCase {
	return []dbMaxConnsCase{
		{"unset", "", dsnPlain, 20, "default", 0},
		{"env 12", "12", dsnPlain, 12, "env", 0},
		{"env zero", "0", dsnPlain, 20, "default", 1},
		{"env negative", "-3", dsnPlain, 20, "default", 1},
		{"env trailing junk", "12x", dsnPlain, 20, "default", 1},
		{"env overflows int32", "2147483648", dsnPlain, 20, "default", 1},
		{"url dsn", "", dsnURL5, 5, "dsn", 0},
		{"keyword dsn", "", dsnKV5, 5, "dsn", 0},
		{"env beats dsn", "12", dsnURL5, 12, "env", 0},
		{"bad env falls to dsn", "abc", dsnURL5, 5, "dsn", 1},
		{"invalid dsn param", "", dsnBadParam, 0, "dsn", 0},
	}
}

func TestResolveDBMaxConns(t *testing.T) {
	for _, tc := range dbMaxConnsCases() {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })
			got, src := resolveDBMaxConns(tc.env, tc.dsn)
			if got != tc.want || src != tc.wantSource {
				t.Errorf("resolveDBMaxConns(%q) = (%d, %q), want (%d, %q)", tc.env, got, src, tc.want, tc.wantSource)
			}
			if n := strings.Count(logs.String(), "DB_MAX_CONNS"); n != tc.wantWarn {
				t.Errorf("DB_MAX_CONNS warnings = %d, want %d; log: %s", n, tc.wantWarn, logs.String())
			}
		})
	}
}

// TestResolveDBMaxConnsDefaultIgnoresNumCPU pins the default as a constant, not a function of
// the host's CPU count (pgx's own default is max(4, NumCPU)).
func TestResolveDBMaxConnsDefaultIgnoresNumCPU(t *testing.T) {
	t.Logf("runtime.NumCPU() = %d", runtime.NumCPU())
	got, src := resolveDBMaxConns("", "postgres://u:p@h:5432/d")
	if got != 20 || src != "default" {
		t.Errorf("resolveDBMaxConns = (%d, %q), want (20, \"default\")", got, src)
	}
}

func setLoadEnv(t *testing.T, dsn string) {
	t.Helper()
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))
}

func captureLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

func TestLoadDBMaxConns(t *testing.T) {
	for _, tc := range dbMaxConnsCases() {
		t.Run(tc.name, func(t *testing.T) {
			setLoadEnv(t, tc.dsn)
			t.Setenv("DB_MAX_CONNS", tc.env)
			logs := captureLogs(t, slog.LevelWarn)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if cfg.DBMaxConns != tc.want || cfg.DBMaxConnsSource != tc.wantSource {
				t.Errorf("DBMaxConns = (%d, %q), want (%d, %q)", cfg.DBMaxConns, cfg.DBMaxConnsSource, tc.want, tc.wantSource)
			}
			if n := strings.Count(logs.String(), "DB_MAX_CONNS"); n != tc.wantWarn {
				t.Errorf("DB_MAX_CONNS warnings = %d, want %d; log: %s", n, tc.wantWarn, logs.String())
			}
		})
	}
}

func TestLoadDBMaxConnsUnparseableDSNDoesNotLeak(t *testing.T) {
	const dsn = "postgres://uzi:zzdsnmarker4418@db:notaport/uzi"
	if _, err := pgconn.ParseConfig(dsn); err == nil {
		t.Fatal("precondition: pgconn.ParseConfig must reject the DSN")
	}
	setLoadEnv(t, dsn)
	t.Setenv("DB_MAX_CONNS", "")
	logs := captureLogs(t, slog.LevelDebug)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.DBMaxConns != 20 || cfg.DBMaxConnsSource != "default" {
		t.Errorf("DBMaxConns = (%d, %q), want (20, \"default\")", cfg.DBMaxConns, cfg.DBMaxConnsSource)
	}
	for _, leak := range []string{"zzdsnmarker4418", "notaport", "postgres://"} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("log output leaks %q: %s", leak, logs.String())
		}
	}
}
