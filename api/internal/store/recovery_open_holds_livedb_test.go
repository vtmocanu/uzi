package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCountOpenHoldsForOwnedRunLiveDB executes CountOpenHoldsForOwnedRun (issue #3431) against a
// real Postgres: the count is run-wide (every worker, every generation), counts only state
// 'open', and is authorized on runs.user_id.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run it via
// e2e/run-store-it.sh. A package that prints ok with PASS=0 is INVALID, not green.
func TestCountOpenHoldsForOwnedRunLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	owner, other, connID, repoID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	for _, u := range []uuid.UUID{owner, other} {
		exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, u, fmt.Sprintf("ropen-%s@e2e", u))
	}
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/ropen', 'https://forge.e2e/g/ropen', 'main', true)`, repoID, connID)

	newRun := func(user uuid.UUID, status string, iid int) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'c', $5)`, id, user, repoID, iid, status)
		return id
	}
	// addHold inserts a hold taken by an arbitrary original worker at the given generation.
	addHold := func(run, user uuid.UUID, generation int, state string) {
		t.Helper()
		switch state {
		case "open":
			exec(`INSERT INTO recovery_custody_holds (id,user_id,run_id,generation,state,original_worker_id,original_worker_identity)
			      VALUES ($1,$2,$3,$4,'open',$5,'fixture')`, uuid.New(), user, run, generation, uuid.New())
		case "discarded":
			exec(`INSERT INTO recovery_custody_holds (id,user_id,run_id,generation,state,original_worker_id,original_worker_identity,release_evidence,released_at)
			      VALUES ($1,$2,$3,$4,'discarded',$5,'fixture','owner_discard',now())`, uuid.New(), user, run, generation, uuid.New())
		case "released":
			exec(`INSERT INTO recovery_custody_holds (id,user_id,run_id,generation,state,original_worker_id,original_worker_identity,released_at)
			      VALUES ($1,$2,$3,$4,'released',$5,'fixture',now())`, uuid.New(), user, run, generation, uuid.New())
		default:
			t.Fatalf("unknown state %q", state)
		}
	}
	count := func(run, user uuid.UUID) (store.CountOpenHoldsForOwnedRunRow, error) {
		return q.CountOpenHoldsForOwnedRun(ctx, store.CountOpenHoldsForOwnedRunParams{RunID: run, UserID: user})
	}

	// Several workers across several generations are summed; released and discarded are not.
	mixed := newRun(owner, "recovery_wait", 1)
	for gen, state := range []string{"open", "open", "open", "released", "discarded", "released"} {
		addHold(mixed, owner, gen+1, state)
	}
	row, err := count(mixed, owner)
	if err != nil {
		t.Fatalf("count mixed: %v", err)
	}
	if row.RunID != mixed || row.OpenHolds != 3 {
		t.Fatalf("mixed run row = %+v, want run %s with exactly 3 open holds", row, mixed)
	}

	// A hold stamped with a different user_id still counts: the count carries no hold-level
	// owner filter, authorization is runs.user_id alone.
	addHold(mixed, other, 7, "open")
	if row, err = count(mixed, owner); err != nil || row.OpenHolds != 4 {
		t.Fatalf("count with foreign-stamped hold = %+v, %v; want 4", row, err)
	}

	// A terminal run with only closed holds reports a real zero, not an absent row.
	done := newRun(owner, "completed", 2)
	addHold(done, owner, 1, "released")
	addHold(done, owner, 2, "discarded")
	if row, err = count(done, owner); err != nil || row.RunID != done || row.OpenHolds != 0 {
		t.Fatalf("terminal zero = %+v, %v; want run %s with 0 open holds", row, err, done)
	}

	// Another user's run yields no row, whatever it holds.
	if _, err = count(mixed, other); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign user's run = %v, want pgx.ErrNoRows", err)
	}
	// An unknown run yields no row.
	if _, err = count(uuid.New(), owner); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown run = %v, want pgx.ErrNoRows", err)
	}
}
