package store_test

// PRD #517 M4 — the graceful-stop wind-down at the SQL layer, against a REAL Postgres.
// The M4 mechanism is: `uzi run stop` enqueues a kind='stop' input AND stamps
// runs.stop_kind='stopped' in one CreateStopVerdictInput CTE (never a server-side
// transition); the worker then finalizes and reports `completed` via SetRunCompleted,
// which must LEAVE the pre-stamped stop_kind intact so the run lands as completed-with-
// stopped. Neither half is visible to sqlc generate + go build + go vet (sqlc's type
// deduction is not Postgres's), so this executes the statements.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// CreateStopVerdictInput with kind='stop', stop_kind='stopped' stamps runs.stop_kind
// AND inserts a 'stop' input row; a subsequent SetRunCompleted on that run leaves
// stop_kind='stopped' intact — the land-as-completed-with-stopped mechanism.
//
// MUTATION PROOF: if CreateStopVerdictInput did not stamp stop_kind (e.g. the CTE's
// UPDATE were dropped), the first assert fails; if SetRunCompleted CLEARED stop_kind
// (added `stop_kind = NULL` to its SET list), the second assert fails.
func TestStopVerdictStopsThenCompletesLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupFollowup(ctx, t, dsn)
	defer done()

	wkr := f.seedWorker(ctx, t, "online", pgtype.Int4{}, false)
	// A parked interactive task — the exact shape a graceful stop targets.
	run := f.seedInteractiveTaskRun(ctx, t, "awaiting_followup", &wkr)

	// ── stop: the dedicated CTE stamps stop_kind='stopped' AND enqueues the input. ──
	if _, err := f.q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
		RunID: run, Kind: "stop", Body: pgT("wind down please"),
		StopKind: pgtype.Text{String: "stopped", Valid: true}, StopReason: pgT("wind down please"),
	}); err != nil {
		t.Fatalf("CreateStopVerdictInput(stop): %v", err)
	}

	got, err := f.q.GetRunByID(ctx, run)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if !got.StopKind.Valid || got.StopKind.String != "stopped" {
		t.Fatalf("stop must stamp stop_kind='stopped', got %v — the CreateStopVerdictInput UPDATE did not land", got.StopKind)
	}
	// The stop must NOT itself transition the run server-side: it stays parked.
	if got.Status != "awaiting_followup" {
		t.Fatalf("a stop transitioned the run server-side to %q, want awaiting_followup — stop must only enqueue+stamp", got.Status)
	}

	// The 'stop' input row is present and unconsumed (the worker's poll will consume it).
	var kind string
	var consumed pgtype.Timestamptz
	if err := f.pool.QueryRow(ctx,
		`SELECT kind, consumed_at FROM run_user_inputs WHERE run_id = $1`, run,
	).Scan(&kind, &consumed); err != nil {
		t.Fatalf("select run_user_inputs: %v", err)
	}
	if kind != "stop" {
		t.Fatalf("enqueued input kind = %q, want stop", kind)
	}
	if consumed.Valid {
		t.Fatalf("the stop input was already consumed_at %v — it must ride to the worker unconsumed", consumed.Time)
	}

	// ── worker finalizes: SetRunCompleted admits awaiting_followup and must NOT clear
	//    the pre-stamped stop_kind. ──
	rows, err := f.q.SetRunCompleted(ctx, store.SetRunCompletedParams{
		ID: run, WorkerID: pgU(wkr),
		Branch: pgT("uzi/task/" + run.String()), MrIid: pgtype.Int8{Int64: 7, Valid: true},
		MrWebUrl: pgT("https://forge.e2e/g/r/-/merge_requests/7"),
	})
	if err != nil {
		t.Fatalf("SetRunCompleted: %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunCompleted affected %d rows, want 1 — awaiting_followup must be admitted to completed", rows)
	}
	after, err := f.q.GetRunByID(ctx, run)
	if err != nil {
		t.Fatalf("GetRunByID(after): %v", err)
	}
	if after.Status != "completed" {
		t.Fatalf("status = %q after SetRunCompleted, want completed", after.Status)
	}
	if !after.StopKind.Valid || after.StopKind.String != "stopped" {
		t.Fatalf("SetRunCompleted CLEARED stop_kind (got %v) — the completed run must retain stop_kind='stopped' "+
			"so the stop disposition survives the terminal transition", after.StopKind)
	}
}

// A defense-in-depth read: the run_user_inputs.kind CHECK admits 'stop' (migration
// 00144). A raw insert of a bogus kind is rejected, so the constraint is live.
func TestStopInputKindConstraintLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupFollowup(ctx, t, dsn)
	defer done()

	wkr := f.seedWorker(ctx, t, "online", pgtype.Int4{}, false)
	run := f.seedInteractiveTaskRun(ctx, t, "awaiting_followup", &wkr)

	// 'stop' is accepted. (run_user_inputs.id is an auto-generated bigint — do not
	// supply it; let the default fill it.)
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'stop', 'x')`,
		run,
	); err != nil {
		t.Fatalf("a 'stop' input must be accepted by the CHECK: %v", err)
	}
	// A bogus kind is rejected.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'bogus', 'x')`,
		run,
	); err == nil {
		t.Error("an out-of-domain run_user_inputs.kind must violate the CHECK constraint")
	}
}
