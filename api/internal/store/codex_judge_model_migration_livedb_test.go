package store

import (
	"context"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
)

// PRD #3245: 00348_codex_judge_model.sql copies a curated judge_model into the
// new Codex lane (users and the instance key) and leaves the Claude lane untouched. The
// SQL literal list must equal agenttmpl.CuratedCodexModels, which is itself pinned to
// fixtures/codex-models/models.json, so this test iterates that set.

// codexJudgeMigrationDB stands a throwaway database at the given version and returns its
// pool and DSN. The migration version is derived from the file name, so a renumber needs no edit.
func codexJudgeMigrationDB(t *testing.T, version int64) (*pgxpool.Pool, string) {
	t.Helper()
	_, dsn := cjCreateDatabase(t, "")
	if err := MigrateTo(context.Background(), dsn, version); err != nil {
		t.Fatalf("MigrateTo(%d): %v", version, err)
	}
	return cjOpenPool(t, dsn), dsn
}

// cjCreateDatabase owns only the generated database; template, when supplied,
// names the historical fixture created by newCJMigrationTemplate.
func cjCreateDatabase(t *testing.T, template string) (string, string) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store-IT runner for live-DB coverage")
	}
	ctx := context.Background()
	name := "codex_judge_mig_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	adminPool, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	create := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if template != "" {
		create += " TEMPLATE " + pgx.Identifier{template}.Sanitize() + " ALLOW_CONNECTIONS true"
	}
	if _, err := adminPool.Exec(ctx, create); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		cleanup, err := OpenPool(ctx, dsn)
		if err != nil {
			t.Errorf("cleanup: open admin pool to drop %s: %v", name, err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("cleanup: drop database %s: %v", name, err)
			return
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	return name, u.String()
}

func cjOpenPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := OpenPool(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type cjMigrationTemplate struct {
	name    string
	version int64
}

func newCJMigrationTemplate(t *testing.T, version int64) cjMigrationTemplate {
	t.Helper()
	name, dsn := cjCreateDatabase(t, "")
	ctx := context.Background()
	if err := MigrateTo(ctx, dsn, version); err != nil {
		t.Fatalf("MigrateTo(%d): %v", version, err)
	}
	pool := cjOpenPool(t, dsn)
	cjAssertHistorical(t, pool, version)
	for _, key := range []string{"judge_model", "codex_judge_model"} {
		if _, ok := cjAppValue(t, pool, key); ok {
			t.Fatalf("historical template already has app_settings key %s", key)
		}
	}
	// OpenPool pings the database; close its connections before sealing it.
	// MigrateTo closes its own SQL handle before returning.
	pool.Close()
	adminPool, err := OpenPool(ctx, os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("open admin pool to seal template: %v", err)
	}
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{name}.Sanitize()+" ALLOW_CONNECTIONS false"); err != nil {
		t.Fatalf("seal historical template %s: %v", name, err)
	}
	return cjMigrationTemplate{name: name, version: version}
}

func (fixture cjMigrationTemplate) Clone(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	_, dsn := cjCreateDatabase(t, fixture.name)
	pool := cjOpenPool(t, dsn)
	cjAssertHistorical(t, pool, fixture.version)
	return pool, dsn
}

func cjAssertHistorical(t *testing.T, pool *pgxpool.Pool, version int64) {
	t.Helper()
	var applied int64
	var higher int
	if err := pool.QueryRow(context.Background(), `SELECT
		COALESCE(max(version_id) FILTER (WHERE is_applied), 0),
		count(*) FILTER (WHERE is_applied AND version_id > $1)
		FROM goose_db_version`, version).Scan(&applied, &higher); err != nil {
		t.Fatalf("read historical migration version: %v", err)
	}
	if applied != version || higher != 0 {
		t.Fatalf("historical migration version = %d with %d applied higher, want %d and none higher", applied, higher, version)
	}
	if cjCodexColumns(t, pool) != 0 {
		t.Fatalf("codex_judge_model already exists at v%d; the test would be vacuous", version)
	}
}

func cjExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func cjSeedUser(t *testing.T, pool *pgxpool.Pool, judgeModel any) uuid.UUID {
	t.Helper()
	id := uuid.New()
	cjExec(t, pool, `INSERT INTO users (id, email, password_hash, judge_model) VALUES ($1, $2, 'x', $3)`,
		id, "cj-"+id.String()+"@e2e", judgeModel)
	return id
}

// cjAppValue returns an app_settings value and whether the row exists.
func cjAppValue(t *testing.T, pool *pgxpool.Pool, key string) (string, bool) {
	t.Helper()
	var v string
	err := pool.QueryRow(context.Background(), `SELECT value FROM app_settings WHERE key = $1`, key).Scan(&v)
	if err == pgx.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("read app_settings %s: %v", key, err)
	}
	return v, true
}

func cjCodexColumns(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'codex_judge_model'`).Scan(&n); err != nil {
		t.Fatalf("count users.codex_judge_model: %v", err)
	}
	return n
}

func curatedIDs() []string {
	ids := make([]string, 0, len(agenttmpl.CuratedCodexModels))
	for id := range agenttmpl.CuratedCodexModels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func TestCodexJudgeModelMigrationUsersLiveDB(t *testing.T) {
	ctx := context.Background()
	v := migrationVersionByName(t, "codex_judge_model")
	pool, dsn := codexJudgeMigrationDB(t, v-1)
	if cjCodexColumns(t, pool) != 0 {
		t.Fatalf("codex_judge_model already exists at v%d; the test would be vacuous", v-1)
	}

	type seeded struct {
		id       uuid.UUID
		stored   any
		wantCopy any // nil = column stays NULL
	}
	var rows []seeded
	for _, id := range curatedIDs() {
		for _, padded := range []string{id, "  " + id + "  ", "\t" + id + "\n", id + "\v"} {
			rows = append(rows, seeded{cjSeedUser(t, pool, padded), padded, id})
		}
	}
	for _, notCurated := range []string{"gpt-6-luna", "opus", "", "gpt-7-x", "gpt-6-sol extra", "vgpt-6-sol"} {
		rows = append(rows, seeded{cjSeedUser(t, pool, notCurated), notCurated, nil})
	}
	rows = append(rows, seeded{cjSeedUser(t, pool, nil), nil, nil})

	if err := MigrateTo(ctx, dsn, v); err != nil {
		t.Fatalf("MigrateTo(%d): %v", v, err)
	}
	for _, r := range rows {
		var claude, codex *string
		if err := pool.QueryRow(ctx, `SELECT judge_model, codex_judge_model FROM users WHERE id = $1`, r.id).Scan(&claude, &codex); err != nil {
			t.Fatalf("read user: %v", err)
		}
		switch stored := r.stored.(type) {
		case nil:
			if claude != nil {
				t.Errorf("NULL judge_model became %q", *claude)
			}
		case string:
			if claude == nil || *claude != stored {
				t.Errorf("judge_model for %q changed: %v (the Claude lane must be untouched)", stored, claude)
			}
		}
		if r.wantCopy == nil {
			if codex != nil {
				t.Errorf("judge_model %q was copied to codex_judge_model %q, want NULL", r.stored, *codex)
			}
		} else if codex == nil || *codex != r.wantCopy.(string) {
			t.Errorf("judge_model %q: codex_judge_model = %v, want %q", r.stored, codex, r.wantCopy)
		}
	}
}

func TestCodexJudgeModelMigrationInstanceLiveDB(t *testing.T) {
	ctx := context.Background()
	v := migrationVersionByName(t, "codex_judge_model")
	fixture := newCJMigrationTemplate(t, v-1)
	updatedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	t.Run("curated padded value is copied trimmed with its author and time", func(t *testing.T) {
		for _, id := range curatedIDs() {
			pool, dsn := fixture.Clone(t)
			admin := cjSeedUser(t, pool, nil)
			raw := "\t" + id + " \n\v"
			cjExec(t, pool, `INSERT INTO app_settings (key, value, updated_by, updated_at) VALUES ('judge_model', $1, $2, $3)`, raw, admin, updatedAt)
			if err := MigrateTo(ctx, dsn, v); err != nil {
				t.Fatalf("MigrateTo(%d): %v", v, err)
			}
			if got, ok := cjAppValue(t, pool, "codex_judge_model"); !ok || got != id {
				t.Errorf("codex_judge_model = %q (exists %v), want %q", got, ok, id)
			}
			if got, _ := cjAppValue(t, pool, "judge_model"); got != raw {
				t.Errorf("judge_model row changed: %q, want byte-for-byte %q", got, raw)
			}
			var by uuid.UUID
			var at time.Time
			if err := pool.QueryRow(ctx, `SELECT updated_by, updated_at FROM app_settings WHERE key = 'codex_judge_model'`).Scan(&by, &at); err != nil {
				t.Fatalf("read copied row: %v", err)
			}
			if by != admin || !at.Equal(updatedAt) {
				t.Errorf("copied row author/time = %v / %v, want %v / %v", by, at, admin, updatedAt)
			}
		}
	})

	for _, notCopied := range []string{"gpt-6-luna", "opus", "", "vgpt-6-sol"} {
		t.Run("no Codex row for "+notCopied, func(t *testing.T) {
			pool, dsn := fixture.Clone(t)
			cjExec(t, pool, `INSERT INTO app_settings (key, value) VALUES ('judge_model', $1)`, notCopied)
			if err := MigrateTo(ctx, dsn, v); err != nil {
				t.Fatalf("MigrateTo(%d): %v", v, err)
			}
			if got, ok := cjAppValue(t, pool, "codex_judge_model"); ok {
				t.Errorf("unexpected codex_judge_model row %q", got)
			}
			if got, _ := cjAppValue(t, pool, "judge_model"); got != notCopied {
				t.Errorf("judge_model row changed: %q", got)
			}
		})
	}

	t.Run("an existing codex_judge_model row is not overwritten", func(t *testing.T) {
		pool, dsn := fixture.Clone(t)
		cjExec(t, pool, `INSERT INTO app_settings (key, value) VALUES ('judge_model', 'gpt-6-sol'), ('codex_judge_model', 'gpt-6-astra')`)
		if err := MigrateTo(ctx, dsn, v); err != nil {
			t.Fatalf("MigrateTo(%d): %v", v, err)
		}
		if got, _ := cjAppValue(t, pool, "codex_judge_model"); got != "gpt-6-astra" {
			t.Errorf("codex_judge_model = %q, want the existing gpt-6-astra", got)
		}
	})
}

func TestCodexJudgeModelMigrationDownLiveDB(t *testing.T) {
	ctx := context.Background()
	v := migrationVersionByName(t, "codex_judge_model")

	for _, tc := range []struct {
		name      string
		claude    *string // existing judge_model row; nil = absent
		codex     string
		want      string
		wantExist bool
		author    bool // seed the codex row with a non-NULL updated_by
	}{
		{"absent Claude row receives the Codex value", nil, "gpt-6-sol", "gpt-6-sol", true, true},
		{"reset Claude row is updated", ptrStr(""), "gpt-6-sol", "gpt-6-sol", true, false},
		{"non-empty Claude value is not overwritten", ptrStr("sonnet"), "gpt-6-sol", "sonnet", true, false},
		{"empty Codex row writes nothing", nil, "", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, dsn := codexJudgeMigrationDB(t, v)
			var authorID any
			if tc.author {
				authorID = cjSeedUser(t, pool, nil)
			}
			cjExec(t, pool, `INSERT INTO app_settings (key, value, updated_by) VALUES ('codex_judge_model', $1, $2)`, tc.codex, authorID)
			if tc.claude != nil {
				cjExec(t, pool, `INSERT INTO app_settings (key, value) VALUES ('judge_model', $1)`, *tc.claude)
			}
			userID := cjSeedUser(t, pool, nil)
			cjExec(t, pool, `UPDATE users SET codex_judge_model = 'gpt-6-astra' WHERE id = $1`, userID)

			if err := MigrateDownTo(ctx, dsn, v-1); err != nil {
				t.Fatalf("MigrateDownTo(%d): %v", v-1, err)
			}
			got, ok := cjAppValue(t, pool, "judge_model")
			if ok != tc.wantExist || got != tc.want {
				t.Errorf("judge_model = %q (exists %v), want %q (exists %v)", got, ok, tc.want, tc.wantExist)
			}
			if tc.author {
				var by *uuid.UUID
				if err := pool.QueryRow(ctx, `SELECT updated_by FROM app_settings WHERE key = 'judge_model'`).Scan(&by); err != nil {
					t.Fatalf("read judge_model updated_by: %v", err)
				}
				if by == nil || authorID == nil || *by != authorID.(uuid.UUID) {
					t.Errorf("judge_model updated_by = %v, want %v", by, authorID)
				}
			}
			if _, ok := cjAppValue(t, pool, "codex_judge_model"); ok {
				t.Errorf("codex_judge_model row survived Down")
			}
			if cjCodexColumns(t, pool) != 0 {
				t.Errorf("users.codex_judge_model survived Down")
			}

			// Down then Up is clean, and re-copies a curated Claude-lane value.
			if err := MigrateTo(ctx, dsn, v); err != nil {
				t.Fatalf("Up after Down: %v", err)
			}
			if cjCodexColumns(t, pool) != 1 {
				t.Errorf("users.codex_judge_model missing after Up")
			}
			if tc.wantExist && tc.want == tc.codex {
				if got, _ := cjAppValue(t, pool, "codex_judge_model"); got != tc.codex {
					t.Errorf("codex_judge_model after re-Up = %q, want %q", got, tc.codex)
				}
			}
		})
	}
}

func ptrStr(s string) *string { return &s }
