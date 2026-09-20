package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestSweepTaskNeverDispatchedLiveDB pins the issue #1367 undispatched-handoff reaper against a
// REAL Postgres: a kind='task' (handoff) run created status='queued' with dispatched_at NULL is
// claimable only once the CLI stamps dispatched_at (DispatchTaskRun). If the push/dispatch never
// lands, the row sits queued+undispatched forever — no other sweep touches it — so
// SweepTaskNeverDispatched terminalizes it past the dispatch grace window with
// fail_origin='task_undispatched'.
//
// The test is DISCRIMINATING by contrast, not a single positive. It seeds four runs and asserts
// the sweep touches EXACTLY the one old, queued, undispatched task:
//   - old undispatched task (created_at well before cutoff, dispatched_at NULL) → swept.
//   - a DISPATCHED task (dispatched_at set) → untouched (the dispatched_at IS NULL conjunct).
//   - a RECENT task (created_at after cutoff) → untouched (the created_at < cutoff conjunct).
//   - a NON-task run (kind='issue') → untouched (the kind = 'task' conjunct).
//
// It then proves the dispatch-vs-expiry race is decided by the new status='queued' guard on
// DispatchTaskRun, in both orderings, plus the guard on a row already failed.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; mirrors the other
// *_livedb / *_integration_test.go in this package.
func TestSweepTaskNeverDispatchedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("sweep-undispatched-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// A queued undispatched task run. kind='task' requires issue_iid NULL + branch set
	// (runs_kind_shape, 00134). `age` sets created_at; dispatched stamps dispatched_at.
	insertTask := func(id uuid.UUID, age time.Duration, dispatched bool) {
		t.Helper()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, branch, issue_title, issue_description, status, created_at,
			     dispatched_at)
			 VALUES ($1, $2, $3, 'task', $4, 't', 'd', 'queued', now() - $5::interval,
			     CASE WHEN $6 THEN now() ELSE NULL END)`,
			id, userID, repoID, "uzi/task/"+id.String(),
			pgtype.Interval{Microseconds: age.Microseconds(), Valid: true}, dispatched)
	}

	oldUndispatched := uuid.New() // old + queued + undispatched → the single sweep target
	dispatched := uuid.New()      // old + queued but dispatched_at set → untouched
	recent := uuid.New()          // queued + undispatched but created after cutoff → untouched
	nonTask := uuid.New()         // kind='issue' → untouched
	cancelledUndisp := uuid.New() // old + undispatched but status='cancelled' → untouched

	insertTask(oldUndispatched, time.Hour, false)
	insertTask(dispatched, time.Hour, true)
	insertTask(recent, time.Minute, false) // 1m old: after a now-15m cutoff
	// A non-queued (cancelled) old undispatched task: it matches every conjunct EXCEPT
	// status='queued', so it isolates that conjunct — the sweep must leave a terminal row
	// alone rather than re-failing a run some other path already settled.
	insertTask(cancelledUndisp, time.Hour, false)
	mustExec(ctx, t, pool, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, cancelledUndisp)
	// A non-task control: kind='issue' needs issue_iid NOT NULL (runs_kind_shape), created old
	// and queued so it differs from the sweep target ONLY in kind.
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, created_at)
		 VALUES ($1, $2, $3, 'issue', 1, 't', 'd', 'queued', now() - interval '1 hour')`,
		nonTask, userID, repoID)

	readStatus := func(id uuid.UUID) string {
		t.Helper()
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("read run %s: %v", id, err)
		}
		return status
	}
	readFailOrigin := func(id uuid.UUID) (string, bool) {
		t.Helper()
		var fo pgtype.Text
		if err := pool.QueryRow(ctx, `SELECT fail_origin FROM runs WHERE id = $1`, id).Scan(&fo); err != nil {
			t.Fatalf("read fail_origin %s: %v", id, err)
		}
		return fo.String, fo.Valid
	}
	readFailureReason := func(id uuid.UUID) string {
		t.Helper()
		var fr pgtype.Text
		if err := pool.QueryRow(ctx, `SELECT failure_reason FROM runs WHERE id = $1`, id).Scan(&fr); err != nil {
			t.Fatalf("read failure_reason %s: %v", id, err)
		}
		return fr.String
	}

	// cutoff = now - 15m (defaultDispatchGrace). The 1h-old runs are before it; the 1m-old run
	// is after it. The 15m gap dwarfs any client/db clock skew.
	const wantReason = "handoff was not dispatched before its setup deadline"
	swept, err := q.SweepTaskNeverDispatched(ctx, store.SweepTaskNeverDispatchedParams{
		FailureReason: pgtype.Text{String: wantReason, Valid: true},
		Cutoff:        pgtype.Timestamptz{Time: time.Now().UTC().Add(-15 * time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("SweepTaskNeverDispatched: %v", err)
	}
	sweptIDs := map[uuid.UUID]bool{}
	for _, s := range swept {
		sweptIDs[s.ID] = true
	}

	// The old undispatched task is the ONLY row swept, and it is fully terminalized.
	if !sweptIDs[oldUndispatched] {
		t.Fatalf("old undispatched task was NOT swept; it is queued+undispatched past the grace window "+
			"and must be failed as task_undispatched: %+v", swept)
	}
	if got := readStatus(oldUndispatched); got != "failed" {
		t.Fatalf("old undispatched task status = %q, want failed", got)
	}
	if fo, ok := readFailOrigin(oldUndispatched); !ok || fo != "task_undispatched" {
		t.Fatalf("old undispatched task fail_origin = %q (valid=%t), want task_undispatched", fo, ok)
	}
	if got := readFailureReason(oldUndispatched); got != wantReason {
		t.Fatalf("old undispatched task failure_reason = %q, want %q", got, wantReason)
	}

	// Each control is untouched — proving a distinct WHERE conjunct, so the sweep is not
	// vacuously failing everything.
	for name, tc := range map[string]struct {
		id         uuid.UUID
		wantStatus string
	}{
		"dispatched (dispatched_at set)": {dispatched, "queued"},
		"recent (created after cutoff)":  {recent, "queued"},
		"non-task (kind='issue')":        {nonTask, "queued"},
		"cancelled (status<>'queued')":   {cancelledUndisp, "cancelled"},
	} {
		if sweptIDs[tc.id] {
			t.Fatalf("%s run was swept; SweepTaskNeverDispatched must leave it alone", name)
		}
		if got := readStatus(tc.id); got != tc.wantStatus {
			t.Fatalf("%s run status = %q after sweep, want %q (untouched)", name, got, tc.wantStatus)
		}
	}

	// ---- Dispatch-vs-expiry race, ordering A: dispatch wins on a fresh queued task ----
	// A brand-new queued undispatched task (created just now, so the reaper's cutoff excludes
	// it). DispatchTaskRun stamps dispatched_at; a following sweep returns 0 rows for it.
	raceA := uuid.New()
	insertTask(raceA, 0, false)
	if _, err := q.DispatchTaskRun(ctx, store.DispatchTaskRunParams{RunID: raceA, UserID: userID}); err != nil {
		t.Fatalf("DispatchTaskRun(raceA): %v", err)
	}
	if got := readStatus(raceA); got != "queued" {
		t.Fatalf("raceA status after dispatch = %q, want queued (dispatch only stamps dispatched_at)", got)
	}
	sweptA, err := q.SweepTaskNeverDispatched(ctx, store.SweepTaskNeverDispatchedParams{
		FailureReason: pgtype.Text{String: wantReason, Valid: true},
		// A cutoff in the FUTURE so raceA's created_at is unambiguously before it — isolating
		// the dispatched_at IS NULL conjunct as the sole reason the dispatched row is spared.
		Cutoff: pgtype.Timestamptz{Time: time.Now().UTC().Add(time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("SweepTaskNeverDispatched(after dispatch): %v", err)
	}
	for _, s := range sweptA {
		if s.ID == raceA {
			t.Fatalf("a dispatched task was swept; the dispatched_at IS NULL conjunct must spare it")
		}
	}
	if got := readStatus(raceA); got != "queued" {
		t.Fatalf("raceA status = %q after sweep, want queued (dispatched, so not reaped)", got)
	}

	// ---- Dispatch-vs-expiry race, ordering B: sweep wins, late dispatch is a no-op ----
	// An old queued undispatched task the sweep fails first; a LATE DispatchTaskRun must then
	// match 0 rows (pgx.ErrNoRows) because of the new status='queued' guard, rather than
	// stamping dispatched_at onto a failed run.
	raceB := uuid.New()
	insertTask(raceB, time.Hour, false)
	sweptB, err := q.SweepTaskNeverDispatched(ctx, store.SweepTaskNeverDispatchedParams{
		FailureReason: pgtype.Text{String: wantReason, Valid: true},
		Cutoff:        pgtype.Timestamptz{Time: time.Now().UTC().Add(-15 * time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("SweepTaskNeverDispatched(raceB): %v", err)
	}
	sawB := false
	for _, s := range sweptB {
		if s.ID == raceB {
			sawB = true
		}
	}
	if !sawB {
		t.Fatalf("raceB was not swept; it is old+queued+undispatched and must be failed")
	}
	if got := readStatus(raceB); got != "failed" {
		t.Fatalf("raceB status = %q after sweep, want failed", got)
	}
	if _, err := q.DispatchTaskRun(ctx, store.DispatchTaskRunParams{RunID: raceB, UserID: userID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("late DispatchTaskRun on a failed run returned err=%v, want pgx.ErrNoRows "+
			"(the status='queued' guard must reject a dispatch onto an already-expired run)", err)
	}
	// The late dispatch left the row terminal and undispatched.
	if got := readStatus(raceB); got != "failed" {
		t.Fatalf("raceB status = %q after the rejected late dispatch, want failed (untouched)", got)
	}

	// ---- Dispatch guard alone: DispatchTaskRun on a non-queued task returns pgx.ErrNoRows ----
	failedTask := uuid.New()
	insertTask(failedTask, time.Hour, false)
	mustExec(ctx, t, pool, `UPDATE runs SET status = 'failed' WHERE id = $1`, failedTask)
	if _, err := q.DispatchTaskRun(ctx, store.DispatchTaskRunParams{RunID: failedTask, UserID: userID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("DispatchTaskRun on a status='failed' task returned err=%v, want pgx.ErrNoRows "+
			"(the status='queued' guard must reject it)", err)
	}
}
