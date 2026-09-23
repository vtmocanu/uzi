package workersvc

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// user_per_harness_down_livedb_test.go proves migration 00244's Down projection (PRD #1551
// D2 / decision log 2026-09-23) across the full credential matrix, in an ISOLATED database
// (never the shared store-IT DB). For each cell it computes the EXPECTED active lane with
// the REAL Go resolver (ResolveSettingsHarness against the same database), THEN rolls the
// migration Down and asserts the legacy default_model column equals that lane — so the
// expectation is never hand-coded independently of the resolver the Down SQL mirrors. Every
// user's stored default_model is seeded to a STALE marker before Down, proving Down
// recomputes against current credentials rather than trusting the maintained legacy column.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. Name ends LiveDB so the sweep selects it.

const codexNone = "none"
const codexLinked = "linked"
const codexStaging = "staging"
const codexAPIKey = "apikey"

func TestUserPerHarnessDownProjectionLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()

	name := "harness_down_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	adminPool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		adminPool.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	adminPool.Close()
	t.Cleanup(func() {
		cleanup, err := store.OpenPool(ctx, dsn)
		if err != nil {
			t.Logf("cleanup open: %v", err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup drop: %v", err)
		}
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	newDSN := u.String()

	// Migrate to 244 (lanes present) so we can seed lanes and run the resolver.
	if err := store.MigrateTo(ctx, newDSN, 244); err != nil {
		t.Fatalf("MigrateTo(244): %v", err)
	}
	pool, err := store.OpenPool(ctx, newDSN)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}
	defer pool.Close()
	svc := &Service{q: store.New(pool)}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	type cell struct {
		harness   *string // default_harness: nil = NULL
		anthropic bool
		codex     string
	}
	claude := func() *string { s := "claude"; return &s }
	codex := func() *string { s := "codex"; return &s }

	var cells []cell
	for _, dh := range []*string{nil, claude(), codex()} {
		for _, anth := range []bool{true, false} {
			for _, ck := range []string{codexNone, codexLinked, codexStaging, codexAPIKey} {
				cells = append(cells, cell{harness: dh, anthropic: anth, codex: ck})
			}
		}
	}

	type seeded struct {
		id         uuid.UUID
		claudeLane string
		codexLane  string
		c          cell
	}
	var users []seeded

	for i, c := range cells {
		id := uuid.New()
		exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			id, fmt.Sprintf("down-%d-%s@e2e", i, id))
		if c.anthropic {
			exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with)
			      VALUES ($1, $2, 'anthropic_token', $3, 'ct', 'master')`,
				uuid.New(), id, "anthropic-"+uuid.NewString())
		}
		switch c.codex {
		case codexLinked:
			acct := uuid.New()
			exec(`INSERT INTO codex_provider_account (id, user_id, provider_user_id, workspace_account_id, sealed_login, sealed_with)
			      VALUES ($1, $2, $3, $4, 'x', 'master')`,
				acct, id, "pu-"+uuid.NewString(), "wa-"+uuid.NewString())
			sec := uuid.New()
			exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with, is_default)
			      VALUES ($1, $2, 'codex_auth', $3, 'ct', 'master', true)`,
				sec, id, "codex-linked-"+uuid.NewString())
			exec(`INSERT INTO codex_credential_state (user_secret_id, user_id, status, provider_account_id)
			      VALUES ($1, $2, 'linked', $3)`, sec, id, acct)
		case codexStaging:
			sec := uuid.New()
			exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with, is_default)
			      VALUES ($1, $2, 'codex_auth', $3, 'ct', 'master', true)`,
				sec, id, "codex-staging-"+uuid.NewString())
			exec(`INSERT INTO codex_credential_state (user_secret_id, user_id, status)
			      VALUES ($1, $2, 'staging')`, sec, id)
		case codexAPIKey:
			sec := uuid.New()
			exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with, is_default)
			      VALUES ($1, $2, 'openai_api_key', $3, 'ct', 'master', true)`,
				sec, id, "codex-key-"+uuid.NewString())
			exec(`INSERT INTO codex_credential_state (user_secret_id, user_id, status)
			      VALUES ($1, $2, 'static')`, sec, id)
		case codexNone:
			// no codex credential
		}
		claudeLane := fmt.Sprintf("claude-lane-%d", i)
		codexLane := fmt.Sprintf("codex-lane-%d", i)
		// default_model is seeded to a STALE marker: Down must overwrite it with the current
		// resolver lane, proving Down recomputes rather than trusting the legacy column.
		exec(`UPDATE users SET default_claude_model=$2, default_codex_model=$3, default_model=$4, default_harness=$5 WHERE id=$1`,
			id, claudeLane, codexLane, "stale-legacy-"+uuid.NewString(), c.harness)
		users = append(users, seeded{id: id, claudeLane: claudeLane, codexLane: codexLane, c: c})
	}

	// Compute the EXPECTED active lane per user with the real resolver, BEFORE Down.
	expected := make(map[uuid.UUID]string, len(users))
	for _, s := range users {
		h, err := svc.ResolveSettingsHarness(ctx, s.id)
		if err != nil {
			t.Fatalf("ResolveSettingsHarness(%s): %v", s.id, err)
		}
		if h == HarnessCodex {
			expected[s.id] = s.codexLane
		} else {
			expected[s.id] = s.claudeLane
		}
	}

	// Roll Down: project the active lane into default_model, then drop the two lanes.
	if err := store.MigrateDownTo(ctx, newDSN, 243); err != nil {
		t.Fatalf("MigrateDownTo(243): %v", err)
	}

	// Prove the lanes are gone (Down actually ran its DROP).
	var cols int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema='public' AND table_name='users'
		   AND column_name IN ('default_claude_model','default_codex_model')`).Scan(&cols); err != nil {
		t.Fatalf("probe lane columns after Down: %v", err)
	}
	if cols != 0 {
		t.Fatalf("lane columns still present after Down (found %d)", cols)
	}

	for _, s := range users {
		var got *string
		if err := pool.QueryRow(ctx, `SELECT default_model FROM users WHERE id=$1`, s.id).Scan(&got); err != nil {
			t.Fatalf("read default_model(%s): %v", s.id, err)
		}
		want := expected[s.id]
		if got == nil || *got != want {
			dh := "NULL"
			if s.c.harness != nil {
				dh = *s.c.harness
			}
			t.Errorf("Down projection for cell{harness=%s anthropic=%v codex=%s}: default_model=%v, want the resolver's active lane %q",
				dh, s.c.anthropic, s.c.codex, got, want)
		}
	}
}
