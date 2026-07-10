package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestCreateRunInputStopKindLiveDB exercises the PRD #33 deliberate-stop stamp
// against a REAL Postgres — the exact surface the fake-store unit tests cannot
// cover, because they never run the SQL. It proves the two-query split (Decision 3):
// the plain CreateRunInput (approve_plan/follow_up) never touches the runs row, while
// the dedicated CreateStopVerdictInput CTE enqueues the verdict AND stamps stop_kind
// in one statement. It is also the regression guard for the data-modifying-CTE type
// hazard the e2e first caught (an earlier single-query design with a `stop_kind IS
// NOT NULL` guard failed to prepare, SQLSTATE 42P08, and 500'd every approve_plan).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the e2e
// runner (e2e/run-store-it.sh) provides one.
func TestCreateRunInputStopKindLiveDB(t *testing.T) {
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("stopkind-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// seedRun inserts a fresh awaiting_approval issue run and returns its id.
	seedRun := func(iid int64) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'awaiting_approval')`, id, userID, repoID, iid)
		return id
	}
	stopKindOf := func(id uuid.UUID) *string {
		run, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatalf("GetRunByID(%s): %v", id, err)
		}
		if !run.StopKind.Valid {
			return nil
		}
		s := run.StopKind.String
		return &s
	}

	// ── approve_plan: the plain CreateRunInput enqueues without touching the runs
	//    row, so stop_kind stays NULL. ──
	rApprove := seedRun(1)
	if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: rApprove, Kind: "approve_plan", Body: pgtype.Text{String: "", Valid: false},
	}); err != nil {
		t.Fatalf("CreateRunInput(approve_plan) must succeed, got: %v", err)
	}
	if sk := stopKindOf(rApprove); sk != nil {
		t.Fatalf("approve_plan must leave stop_kind NULL, got %q", *sk)
	}

	// ── live cancel: the dedicated CreateStopVerdictInput CTE stamps
	//    stop_kind='cancelled' in the same statement as the input. ──
	rCancel := seedRun(2)
	if _, err := q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
		RunID: rCancel, Kind: "cancel", Body: pgtype.Text{}, StopKind: pgtype.Text{String: "cancelled", Valid: true},
	}); err != nil {
		t.Fatalf("CreateStopVerdictInput(cancel): %v", err)
	}
	if sk := stopKindOf(rCancel); sk == nil || *sk != "cancelled" {
		t.Fatalf("live cancel must stamp stop_kind 'cancelled', got %v", sk)
	}

	// ── live reject: stamps stop_kind='plan_rejected'. ──
	rReject := seedRun(3)
	if _, err := q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
		RunID: rReject, Kind: "reject_plan", Body: pgtype.Text{String: "wrong", Valid: true},
		StopKind: pgtype.Text{String: "plan_rejected", Valid: true},
	}); err != nil {
		t.Fatalf("CreateStopVerdictInput(reject_plan): %v", err)
	}
	if sk := stopKindOf(rReject); sk == nil || *sk != "plan_rejected" {
		t.Fatalf("live reject must stamp stop_kind 'plan_rejected', got %v", sk)
	}

	// ── server-side reject: status failed + stop_kind 'plan_rejected' in one write. ──
	rSrvReject := seedRun(4)
	if _, err := q.RejectRunServerSide(ctx, store.RejectRunServerSideParams{
		ID: rSrvReject, UserID: userID, FailureReason: pgtype.Text{String: "plan rejected", Valid: true},
	}); err != nil {
		t.Fatalf("RejectRunServerSide: %v", err)
	}
	if run, _ := q.GetRunByID(ctx, rSrvReject); run.Status != "failed" || run.StopKind.String != "plan_rejected" {
		t.Fatalf("server-side reject: want failed/plan_rejected, got %s/%v", run.Status, run.StopKind)
	}

	// ── server-side cancel: status cancelled + stop_kind 'cancelled'. ──
	rSrvCancel := seedRun(5)
	if _, err := q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{ID: rSrvCancel, UserID: userID}); err != nil {
		t.Fatalf("CancelRunServerSide: %v", err)
	}
	if run, _ := q.GetRunByID(ctx, rSrvCancel); run.Status != "cancelled" || run.StopKind.String != "cancelled" {
		t.Fatalf("server-side cancel: want cancelled/cancelled, got %s/%v", run.Status, run.StopKind)
	}

	// ── the CHECK constraint rejects an out-of-domain stop_kind value. ──
	rBad := seedRun(6)
	if _, err := pool.Exec(ctx, `UPDATE runs SET stop_kind = 'bogus' WHERE id = $1`, rBad); err == nil {
		t.Error("an out-of-domain stop_kind must violate the CHECK constraint")
	}
}
