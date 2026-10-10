package store

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestScheduleRecentFiresBackfillLiveDB (issue #2519) is the regression gate for the data
// backfill in 00318_schedule_recent_fires.sql: an existing row whose last_fire dispatched
// (matched > 0) gets recent_fires = [last_fire]; a capacity-blocked or examined-0 last_fire,
// and a NULL one, get []. Every other live-DB test reaches the schema at HEAD, so the
// backfill only ever sees an empty table there; this test stands a throwaway database at
// the version before the migration, seeds rows with raw SQL, then applies the migration.
// See TestUserAppearanceBackfillLiveDB for the same pattern.
func TestScheduleRecentFiresBackfillLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store-IT runner for live-DB coverage")
	}
	ctx := context.Background()
	// Derive the versions from the embedded migration name so a renumber on landing needs no edit here.
	recentFiresVersion := migrationVersionByName(t, "schedule_recent_fires")
	preRecentFiresVersion := recentFiresVersion - 1

	name := "recent_fires_bf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	adminPool, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		adminPool.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	adminPool.Close()

	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		cleanupAdmin, err := OpenPool(ctx, dsn)
		if err != nil {
			t.Logf("cleanup: open admin pool to drop %s: %v", name, err)
			return
		}
		defer cleanupAdmin.Close()
		if _, err := cleanupAdmin.Exec(ctx,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup: drop database %s: %v", name, err)
		}
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	newDSN := u.String()

	if err := MigrateTo(ctx, newDSN, preRecentFiresVersion); err != nil {
		t.Fatalf("MigrateTo(%d): %v", preRecentFiresVersion, err)
	}
	pool, err = OpenPool(ctx, newDSN)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}

	var colCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'run_schedules' AND column_name = 'recent_fires'`).Scan(&colCount); err != nil {
		t.Fatalf("probe recent_fires column: %v", err)
	}
	if colCount != 0 {
		t.Fatalf("recent_fires already exists at v%d; the seam over-migrated and the test would be vacuous", preRecentFiresVersion)
	}

	bfExec := func(ctx context.Context, t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	bfExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, "recent-fires-"+userID.String()+"@e2e")
	bfExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot-rf', 7102, $3)`, connID, userID, []byte{0x1})
	bfExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 7102, 'g/rf', 'https://forge.e2e/g/rf', true)`, repoID, connID)

	dispatching := `{"fired_at":"2026-08-12T09:00:00Z","matched":2,"started":[{"issue_iid":7,"run_id":"r1","title":"Ship it"}],"skips":[]}`
	blocked := `{"fired_at":"2026-08-12T10:00:00Z","matched":0,"started":[],"skips":[],"capacity":{"blocked":true}}`
	examined0 := `{"fired_at":"2026-08-12T11:00:00Z","matched":0,"started":[],"skips":[]}`
	seed := func(lastFire any) uuid.UUID {
		id := uuid.New()
		bfExec(ctx, t, pool,
			`INSERT INTO run_schedules (id, user_id, repo_id, target, timing, cron_expr, timezone, next_fire_at, auto_approve, enabled, last_fire)
			 VALUES ($1, $2, $3, 'sweep', 'recurring', '*/5 * * * *', 'UTC', now(), true, true, $4::jsonb)`,
			id, userID, repoID, lastFire)
		return id
	}
	dispatchID := seed(dispatching)
	blockedID := seed(blocked)
	examinedID := seed(examined0)
	nullID := seed(nil)

	if err := MigrateTo(ctx, newDSN, recentFiresVersion); err != nil {
		t.Fatalf("MigrateTo(%d): %v", recentFiresVersion, err)
	}

	read := func(id uuid.UUID) any {
		var raw []byte
		if err := pool.QueryRow(ctx, `SELECT recent_fires FROM run_schedules WHERE id = $1`, id).Scan(&raw); err != nil {
			t.Fatalf("read recent_fires: %v", err)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("recent_fires not JSON: %v (%s)", err, raw)
		}
		return v
	}
	var wantDispatch any
	if err := json.Unmarshal([]byte("["+dispatching+"]"), &wantDispatch); err != nil {
		t.Fatal(err)
	}
	if got := read(dispatchID); !reflect.DeepEqual(got, wantDispatch) {
		t.Errorf("dispatching row recent_fires = %v, want [last_fire] %v", got, wantDispatch)
	}
	empty := []any{}
	for label, id := range map[string]uuid.UUID{"capacity-blocked": blockedID, "examined-0": examinedID, "NULL last_fire": nullID} {
		if got := read(id); !reflect.DeepEqual(got, empty) {
			t.Errorf("%s row recent_fires = %v, want []", label, got)
		}
	}
}
