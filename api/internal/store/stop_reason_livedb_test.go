package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestStopReasonLiveDB is the live-DB gate for PRD #503 M3's new nullable runs.stop_reason
// column (migration 00143) and the two writers that stamp it: CancelRunServerSide (the
// server-side cancel) and CreateStopVerdictInput (the shared live-path CTE). It exercises
// what the fake-store unit tests structurally cannot: that the generated statements actually
// run against a real Postgres and land the operator's optional cancel reason on the row,
// while a NULL param leaves the column NULL.
//
// It lives in the store package DELIBERATELY: e2e/run-store-it.sh and the CI
// test-api-store-it job run `-run 'LiveDB$'` over ./internal/store/... and
// ./internal/handler/... ONLY, so a *LiveDB test placed in workersvc would never gate.
//
// Positive control (non-vacuity): CancelRunServerSide with a WRONG user id must move 0 rows
// AND leave stop_reason NULL, proving the reason lands because the row matched, not because
// the UPDATE is unconditional.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// provides one. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestStopReasonLiveDB(t *testing.T) {
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("sr-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/sr', 'https://forge.e2e/g/sr', 'main', true)`, repoID, connID)

	// Each run gets a UNIQUE issue_iid: uq_runs_one_active_per_issue forbids two active
	// runs on the same (repo, issue) and every run seeded here is non-terminal.
	iid := int32(0)
	seedRun := func(status string) uuid.UUID {
		id := uuid.New()
		iid++
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		      VALUES ($1, $2, $3, 'issue', $4, 'do x', 'ctx', $5)`, id, userID, repoID, iid, status)
		return id
	}
	stopReasonOf := func(id uuid.UUID) pgtype.Text {
		t.Helper()
		var sr pgtype.Text
		if err := pool.QueryRow(ctx, `SELECT stop_reason FROM runs WHERE id = $1`, id).Scan(&sr); err != nil {
			t.Fatalf("select stop_reason: %v", err)
		}
		return sr
	}

	// ── CancelRunServerSide: persists a supplied reason. ──
	const reason = "superseded by a newer run"
	runID := seedRun("queued")

	// Positive control / non-vacuity: a WRONG user id must move 0 rows and stamp nothing.
	rows, err := q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{
		ID: runID, UserID: uuid.New(), StopReason: pgtype.Text{String: reason, Valid: true},
	})
	if err != nil {
		t.Fatalf("CancelRunServerSide(wrong user): %v", err)
	}
	if rows != 0 {
		t.Fatalf("CancelRunServerSide moved %d rows for a non-owning user; the user_id guard is not enforced (vacuous)", rows)
	}
	if sr := stopReasonOf(runID); sr.Valid {
		t.Fatalf("stop_reason = %+v after a non-owning cancel, want it untouched (NULL)", sr)
	}

	// The real transition: the owning user cancels WITH a reason.
	rows, err = q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{
		ID: runID, UserID: userID, StopReason: pgtype.Text{String: reason, Valid: true},
	})
	if err != nil {
		t.Fatalf("CancelRunServerSide(owning user): %v", err)
	}
	if rows != 1 {
		t.Fatalf("CancelRunServerSide moved %d rows, want 1", rows)
	}
	run, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if run.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", run.Status)
	}
	if !run.StopReason.Valid || run.StopReason.String != reason {
		t.Errorf("stop_reason = %+v, want %q", run.StopReason, reason)
	}

	// A server-side cancel with a NULL reason leaves the column NULL (optional reason).
	nullRunID := seedRun("queued")
	if rows, err := q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{
		ID: nullRunID, UserID: userID, StopReason: pgtype.Text{},
	}); err != nil {
		t.Fatalf("CancelRunServerSide(null reason): %v", err)
	} else if rows != 1 {
		t.Fatalf("CancelRunServerSide(null reason) moved %d rows, want 1", rows)
	}
	if sr := stopReasonOf(nullRunID); sr.Valid {
		t.Errorf("stop_reason = %+v after a NULL-reason cancel, want NULL", sr)
	}

	// ── CreateStopVerdictInput: stamps a non-NULL StopReason onto the run row. ──
	stampRunID := seedRun("running")
	if _, err := q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
		RunID:      stampRunID,
		Kind:       "cancel",
		Body:       pgtype.Text{String: reason, Valid: true},
		StopKind:   pgtype.Text{String: "cancelled", Valid: true},
		StopReason: pgtype.Text{String: reason, Valid: true},
	}); err != nil {
		t.Fatalf("CreateStopVerdictInput(with reason): %v", err)
	}
	if sr := stopReasonOf(stampRunID); !sr.Valid || sr.String != reason {
		t.Errorf("stop_reason after CreateStopVerdictInput = %+v, want %q", sr, reason)
	}

	// ── CreateStopVerdictInput: a NULL StopReason leaves the column NULL (reject_plan/auto-stop). ──
	nullStampRunID := seedRun("awaiting_approval")
	if _, err := q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
		RunID:      nullStampRunID,
		Kind:       "reject_plan",
		Body:       pgtype.Text{String: "wrong approach", Valid: true},
		StopKind:   pgtype.Text{String: "plan_rejected", Valid: true},
		StopReason: pgtype.Text{},
	}); err != nil {
		t.Fatalf("CreateStopVerdictInput(null reason): %v", err)
	}
	if sr := stopReasonOf(nullStampRunID); sr.Valid {
		t.Errorf("stop_reason after a NULL-reason stamp = %+v, want NULL (reason belongs to failure_reason)", sr)
	}
}
