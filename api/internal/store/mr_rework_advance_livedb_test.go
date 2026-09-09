package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCreateManualMRReworkRunAndAdvanceLiveDB pins the ATOMIC on-demand (manual) rework
// create+advance query (PRD #1202, review-finding hardening) against a REAL Postgres — the
// properties no fakeStore can prove, because they live in the query's `led` CTE, its
// ON CONFLICT clause, the column DEFAULT, and the cross-kind WHERE NOT EXISTS self-gate:
//
//   - the non-counting ledger advance NEVER spends an automatic cycle: attempt_count is left
//     UNTOUCHED on an existing row and starts at the column DEFAULT (0) on a fresh one — the
//     key difference from UpsertMRReworkLedger, which INSERTs 1 and increments;
//   - high_water is ADVANCE-ONLY (GREATEST), so a smaller value never lowers the mark;
//   - halt_notified is RESET to false, opening a new halt episode (Decision 9);
//   - ATOMIC SUCCESS: a successful call commits BOTH the mr_rework run (kind='mr_rework',
//     trigger_source='manual') AND the ledger advance;
//   - ATOMIC SKIP: with an active ci_fix run occupying the same pipeline_ref, the whole
//     statement is gated — it returns pgx.ErrNoRows (branch-in-use) and NEITHER a new
//     mr_rework run NOR any ledger advance (not even the halt_notified reset) occurs.
//
// 🔴 MUTATION CHECK (the PRD's M1 requirement): if the `led` CTE were pointed at
// UpsertMRReworkLedger's counting body, the attempt_count assertions below read cap+1 and
// redden; if the atomic fold regressed to a create that ignored the branch gate, the ATOMIC
// SKIP assertions redden.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. A package that prints
// `ok` with PASS=0 is INVALID, not green.
func TestCreateManualMRReworkRunAndAdvanceLiveDB(t *testing.T) {
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

	// mr_rework_ledger.repo_id and the run's FKs require a real repo (with its connection and
	// owning user), plus a completed base run to reference as target_run_id (the runs_kind_shape
	// CHECK rejects an mr_rework with a NULL target_run_id). Unique ids keep this isolated in
	// the shared store-it DB.
	user, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user, fmt.Sprintf("adv-%s@e2e", user))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, user, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/adv', 'https://forge.e2e/g/adv', 'main', true)`, repoID, connID)
	targetRun := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	      VALUES ($1, $2, $3, 'issue', 900, 't', 'd', 'completed')`, targetRun, user, repoID)

	const cap = 5
	const ref = "agent/issue-7"

	get := func(atRef string) store.MrReworkLedger {
		t.Helper()
		led, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: atRef})
		if err != nil {
			t.Fatalf("GetMRReworkLedger(%q): %v", atRef, err)
		}
		return led
	}

	// create runs the ATOMIC create+advance for the given ref/high-water. It returns the run
	// and any error, and (on success) marks the created run terminal so a follow-up create on
	// the SAME ref is not blocked by the one-active-branch / one-active-mr_rework partial
	// indexes — this test exercises the ledger across several calls on one ref.
	create := func(atRef string, mrIID, highWater int64) (store.Run, error) {
		t.Helper()
		run, err := q.CreateManualMRReworkRunAndAdvance(ctx, store.CreateManualMRReworkRunAndAdvanceParams{
			UserID:           user,
			RepoID:           repoID,
			IssueTitle:       "Rework MR review (on demand)",
			IssueDescription: "d",
			PipelineRef:      pgtype.Text{String: atRef, Valid: true},
			MrIid:            pgtype.Int8{Int64: mrIID, Valid: true},
			TargetRunID:      pgtype.UUID{Bytes: targetRun, Valid: true},
			ReviewComments:   []byte(`{"comments":[{"id":120}],"truncated":false}`),
			WaitOnLimit:      false,
			HighWater:        highWater,
		})
		if err == nil {
			exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, run.ID)
		}
		return run, err
	}

	countReworkRuns := func(atRef string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM runs WHERE repo_id = $1 AND kind = 'mr_rework' AND pipeline_ref = $2`,
			repoID, atRef).Scan(&n); err != nil {
			t.Fatalf("count mr_rework runs: %v", err)
		}
		return n
	}

	// Pre-seed the post-cap state an owner would rework from: attempt_count AT the cap,
	// high_water 100, halt_notified TRUE (the cap halt already commented once).
	exec(`INSERT INTO mr_rework_ledger (repo_id, ref, attempt_count, high_water, halt_notified)
	      VALUES ($1, $2, $3, 100, true)`, repoID, ref, cap)

	// ── ATOMIC SUCCESS ──────────────────────────────────────────────────────────────────
	// Advance past the mark. The run commits (kind='mr_rework', trigger_source='manual'), AND
	// in the same statement: attempt_count must NOT move (no automatic cycle spent); high_water
	// advances to 200; halt_notified resets to false (new halt episode).
	run, err := create(ref, 55, 200)
	if err != nil {
		t.Fatalf("CreateManualMRReworkRunAndAdvance(200): %v", err)
	}
	if run.Kind != "mr_rework" {
		t.Fatalf("created run kind = %q, want mr_rework", run.Kind)
	}
	if run.TriggerSource != "manual" {
		t.Fatalf("created run trigger_source = %q, want manual (hard-coded in the query)", run.TriggerSource)
	}
	if !run.AutoApprove {
		t.Fatalf("created run auto_approve = %v, want true", run.AutoApprove)
	}
	if countReworkRuns(ref) != 1 {
		t.Fatalf("expected exactly 1 mr_rework run on %q after the atomic create", ref)
	}
	led := get(ref)
	if led.AttemptCount != cap {
		t.Fatalf("attempt_count = %d after a manual advance, want %d (a manual cycle never spends an automatic one)", led.AttemptCount, cap)
	}
	if led.HighWater != 200 {
		t.Fatalf("high_water = %d, want 200 (advanced atomically with the create)", led.HighWater)
	}
	if led.HaltNotified {
		t.Fatal("halt_notified must be reset to false by the manual advance (new halt episode)")
	}

	// ── ADVANCE-ONLY (GREATEST) ─────────────────────────────────────────────────────────
	// A SMALLER value must never lower the mark, and still must not touch the counter. It also
	// re-affirms the halt-latch reset.
	exec(`UPDATE mr_rework_ledger SET halt_notified = true WHERE repo_id = $1 AND ref = $2`, repoID, ref)
	if _, err := create(ref, 56, 50); err != nil {
		t.Fatalf("CreateManualMRReworkRunAndAdvance(50): %v", err)
	}
	led = get(ref)
	if led.HighWater != 200 {
		t.Fatalf("high_water = %d after a smaller advance, want 200 (advance-only, never lowers)", led.HighWater)
	}
	if led.AttemptCount != cap {
		t.Fatalf("attempt_count = %d, want %d (untouched)", led.AttemptCount, cap)
	}
	if led.HaltNotified {
		t.Fatal("halt_notified must be reset even on a no-op GREATEST advance")
	}

	// ── FRESH ROW: attempt_count DEFAULT (0), not 1 ─────────────────────────────────────
	const freshRef = "uzi/prompt-fresh"
	if _, err := create(freshRef, 57, 0); err != nil {
		t.Fatalf("CreateManualMRReworkRunAndAdvance(fresh): %v", err)
	}
	fresh := get(freshRef)
	if fresh.AttemptCount != 0 {
		t.Fatalf("a fresh manual advance started attempt_count at %d, want 0 (the column default, NOT 1)", fresh.AttemptCount)
	}
	if fresh.HighWater != 0 || fresh.HaltNotified {
		t.Fatalf("fresh row wrong: high_water=%d halt_notified=%v, want 0/false", fresh.HighWater, fresh.HaltNotified)
	}

	// ── ATOMIC SKIP: cross-kind branch guard rolls BOTH writes back ─────────────────────
	// An active ci_fix run occupies skipRef's branch (pipeline_ref written AT INSERT). The
	// combined statement self-gates on the SAME WHERE NOT EXISTS in both the run INSERT and
	// the `led` CTE, evaluated on one snapshot, so the whole statement is a no-op: it returns
	// pgx.ErrNoRows and neither a new mr_rework run nor any ledger change (not even the
	// halt_notified reset) lands. mr_iid 700 is distinct from every mr_rework here, so the
	// ONLY blocker is the occupied branch.
	const skipRef = "agent/issue-skip"
	exec(`INSERT INTO mr_rework_ledger (repo_id, ref, attempt_count, high_water, halt_notified)
	      VALUES ($1, $2, 2, 100, true)`, repoID, skipRef)
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_id, pipeline_ref, status)
	      VALUES ($1, $2, $3, 'ci_fix', 't', 'd', 4242, $4, 'running')`, uuid.New(), user, repoID, skipRef)

	if _, err := create(skipRef, 700, 500); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CreateManualMRReworkRunAndAdvance on a ci_fix-occupied branch: err = %v, want pgx.ErrNoRows (WHERE NOT EXISTS → 0 rows)", err)
	}
	if n := countReworkRuns(skipRef); n != 0 {
		t.Fatalf("ATOMIC SKIP created %d mr_rework run(s) on %q, want 0 (the whole statement must roll back)", n, skipRef)
	}
	skipLed := get(skipRef)
	if skipLed.HighWater != 100 {
		t.Fatalf("ATOMIC SKIP advanced high_water to %d, want 100 (the ledger must be untouched)", skipLed.HighWater)
	}
	if skipLed.AttemptCount != 2 {
		t.Fatalf("ATOMIC SKIP moved attempt_count to %d, want 2 (untouched)", skipLed.AttemptCount)
	}
	if !skipLed.HaltNotified {
		t.Fatal("ATOMIC SKIP reset halt_notified; it must stay true (the led CTE must be gated by the same predicate)")
	}
}
