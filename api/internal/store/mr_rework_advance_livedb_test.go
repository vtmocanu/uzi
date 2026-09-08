package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestAdvanceMRReworkHighWaterLiveDB pins the SQL invariant of the on-demand (manual) rework
// ledger write (PRD #1202) against a REAL Postgres — the property no fakeStore can prove,
// because it lives in the query's ON CONFLICT clause and the column DEFAULT:
//
//   - a manual advance NEVER spends an automatic cycle: attempt_count is left UNTOUCHED on
//     an existing row and starts at the column DEFAULT (0) on a fresh one — the key
//     difference from UpsertMRReworkLedger, which INSERTs 1 and increments;
//   - high_water is ADVANCE-ONLY (GREATEST), so a smaller value never lowers the mark;
//   - halt_notified is RESET to false, opening a new halt episode (Decision 9).
//
// 🔴 MUTATION CHECK (the PRD's M1 requirement): if AdvanceMRReworkHighWater were pointed at
// UpsertMRReworkLedger's body, the attempt_count assertions below read cap+1 and redden.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. A package that prints
// `ok` with PASS=0 is INVALID, not green.
func TestAdvanceMRReworkHighWaterLiveDB(t *testing.T) {
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

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	// mr_rework_ledger.repo_id has an FK to repos, so a real repo (with its connection and
	// owning user) must exist. Unique ids keep this isolated in the shared store-it DB.
	user, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user, fmt.Sprintf("adv-%s@e2e", user))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, user, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/adv', 'https://forge.e2e/g/adv', 'main', true)`, repoID, connID)

	const cap = 5
	const ref = "agent/issue-7"

	// Pre-seed the post-cap state an owner would rework from: attempt_count AT the cap,
	// high_water 100, halt_notified TRUE (the cap halt already commented once).
	exec(`INSERT INTO mr_rework_ledger (repo_id, ref, attempt_count, high_water, halt_notified)
	      VALUES ($1, $2, $3, 100, true)`, repoID, ref, cap)

	get := func() store.MrReworkLedger {
		t.Helper()
		led, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: ref})
		if err != nil {
			t.Fatalf("GetMRReworkLedger: %v", err)
		}
		return led
	}

	// Advance past the mark. attempt_count must NOT move (no automatic cycle spent);
	// high_water advances to 200; halt_notified resets to false (new halt episode).
	if err := q.AdvanceMRReworkHighWater(ctx, store.AdvanceMRReworkHighWaterParams{RepoID: repoID, Ref: ref, HighWater: 200}); err != nil {
		t.Fatalf("AdvanceMRReworkHighWater(200): %v", err)
	}
	led := get()
	if led.AttemptCount != cap {
		t.Fatalf("attempt_count = %d after a manual advance, want %d (a manual cycle never spends an automatic one)", led.AttemptCount, cap)
	}
	if led.HighWater != 200 {
		t.Fatalf("high_water = %d, want 200 (advanced)", led.HighWater)
	}
	if led.HaltNotified {
		t.Fatal("halt_notified must be reset to false by the manual advance (new halt episode)")
	}

	// A SMALLER value must never lower the mark (GREATEST), and still must not touch the
	// counter. It also re-affirms the halt-latch reset.
	exec(`UPDATE mr_rework_ledger SET halt_notified = true WHERE repo_id = $1 AND ref = $2`, repoID, ref)
	if err := q.AdvanceMRReworkHighWater(ctx, store.AdvanceMRReworkHighWaterParams{RepoID: repoID, Ref: ref, HighWater: 50}); err != nil {
		t.Fatalf("AdvanceMRReworkHighWater(50): %v", err)
	}
	led = get()
	if led.HighWater != 200 {
		t.Fatalf("high_water = %d after a smaller advance, want 200 (advance-only, never lowers)", led.HighWater)
	}
	if led.AttemptCount != cap {
		t.Fatalf("attempt_count = %d, want %d (untouched)", led.AttemptCount, cap)
	}
	if led.HaltNotified {
		t.Fatal("halt_notified must be reset even on a no-op GREATEST advance")
	}

	// A FRESH ref (no row) INSERTs starting attempt_count at the column DEFAULT (0), NOT 1.
	const freshRef = "uzi/prompt-fresh"
	if err := q.AdvanceMRReworkHighWater(ctx, store.AdvanceMRReworkHighWaterParams{RepoID: repoID, Ref: freshRef, HighWater: 0}); err != nil {
		t.Fatalf("AdvanceMRReworkHighWater(fresh): %v", err)
	}
	fresh, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: freshRef})
	if err != nil {
		t.Fatalf("GetMRReworkLedger(fresh): %v", err)
	}
	if fresh.AttemptCount != 0 {
		t.Fatalf("a fresh manual advance started attempt_count at %d, want 0 (the column default, NOT 1)", fresh.AttemptCount)
	}
	if fresh.HighWater != 0 || fresh.HaltNotified {
		t.Fatalf("fresh row wrong: high_water=%d halt_notified=%v, want 0/false", fresh.HighWater, fresh.HaltNotified)
	}
}
