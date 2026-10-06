package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"

	"github.com/google/uuid"
)

func TestPlanCrossCheckExitSweepLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	childID := uuid.New()
	mustExec(ctx, t, f.pool, `UPDATE runs SET harness = 'claude', plan_cross_check_required = true,
        auto_approve = true, claim_generation = 3, started_at = now() - interval '140 seconds',
        budget_wall_seconds = 60 WHERE id = $1`, f.runID)
	mustExec(ctx, t, f.pool, `UPDATE workers SET last_heartbeat_at = now(),
        protocol_capabilities = ARRAY['wall_park_v1']::text[] WHERE id = $1`, f.workerID)
	mustExec(ctx, t, f.pool, `INSERT INTO runs
        (id, user_id, repo_id, kind, target_run_id, harness, report_only, budget_wall_seconds,
         issue_title, issue_description, status)
        VALUES ($1, $2, $3, 'cross_check', $4, 'codex', true, 1800, 'check', 'check', 'running')`,
		childID, f.userID, f.repoID, f.runID)
	mustExec(ctx, t, f.pool, `INSERT INTO cross_checks
        (lead_run_id, stage, round, lead_claim_generation, plan_md, milestones,
         size_class, base_commit, candidate_digest, checker_run_id, checker_harness,
         created_at, deadline_at)
        VALUES ($1, 'plan', 1, 3, 'plan', '[]', 's', 'base', '\\x01', $2, 'codex',
                now() - interval '120 seconds', now() - interval '5 seconds')`, f.runID, childID)

	wall, err := f.q.RequestWallParks(ctx, store.RequestWallParksParams{
		Now: pgtype.Timestamptz{Time: time.Now(), Valid: true}, GlobalTimeoutSeconds: 60,
		WorkerStaleCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range wall {
		if row.ID == f.runID {
			t.Fatalf("pending wait consumed wall budget: requested=%v", wall)
		}
	}
	// A stale worker uses the server park path. It must honor the same
	// pending-wait credit before the sweep can supersede the row.
	mustExec(ctx, t, f.pool, `UPDATE workers SET last_heartbeat_at = now() - interval '10 minutes' WHERE id = $1`, f.workerID)
	parked, err := f.q.ParkRunsAtWall(ctx, store.ParkRunsAtWallParams{
		Now: pgtype.Timestamptz{Time: time.Now(), Valid: true}, GlobalTimeoutSeconds: 60,
		WorkerStaleCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
		GraceSeconds:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range parked {
		if row.ID == f.runID {
			t.Fatalf("server parked lead before credited deadline: %v", parked)
		}
	}
	mustExec(ctx, t, f.pool, `UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, f.workerID)
	mustExec(ctx, t, f.pool, `UPDATE runs SET completion_attempts = 1,
        completion_contract_version = 1 WHERE id = $1`, f.runID)
	if _, err := f.q.StampCompletionBudgetExhausted(ctx, store.StampCompletionBudgetExhaustedParams{
		Now: pgtype.Timestamptz{Time: time.Now(), Valid: true}, GlobalTimeoutSeconds: 60,
		WorkerStaleCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	var stamped bool
	if err := f.pool.QueryRow(ctx, `SELECT completion_budget_exhausted_at IS NOT NULL
        FROM runs WHERE id = $1`, f.runID).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped {
		t.Fatal("completion budget stamped before credited deadline")
	}
	mustExec(ctx, t, f.pool, `UPDATE runs SET completion_attempts = 0,
        status_since = now() - interval '120 seconds' WHERE id = $1`, f.runID)
	missing, err := f.q.RequeueRunsMissingFromSnapshot(ctx, store.RequeueRunsMissingFromSnapshotParams{
		WorkerID:      pgtype.UUID{Bytes: f.workerID, Valid: true},
		MissingCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
		MaxRequeues:   2, Now: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		GlobalTimeoutSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	foundMissing := false
	for _, row := range missing {
		foundMissing = foundMissing || row.ID == f.runID
	}
	if !foundMissing {
		t.Fatalf("credited lead was excluded from missing-snapshot recovery: %v", missing)
	}
	// Requeue is already a custody exit: settlement must precede this read,
	// without requiring a periodic sweep.
	firstCredit := assertPlanCrossCheckSettled(ctx, t, f, childID, 110)
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running', status_since = now(),
        requeue_count = 0 WHERE id = $1`, f.runID)
	if _, err := f.q.SetRunWallPark(ctx, store.SetRunWallParkParams{
		ID: f.runID, WorkerID: pgtype.UUID{Bytes: f.workerID, Valid: true},
		Now: pgtype.Timestamptz{Time: time.Now(), Valid: true}, GlobalTimeoutSeconds: 60,
		ClaimGeneration: pgtype.Int8{Int64: 3, Valid: true},
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("worker wall park before credited deadline: %v", err)
	}
	// A budget shorter than the non-wait work must still request a park. This
	// proves the no-request assertion above reached the wall decision predicate.
	mustExec(ctx, t, f.pool, `UPDATE runs SET budget_wall_seconds = 1 WHERE id = $1`, f.runID)
	wall, err = f.q.RequestWallParks(ctx, store.RequestWallParksParams{
		Now: pgtype.Timestamptz{Time: time.Now(), Valid: true}, GlobalTimeoutSeconds: 60,
		WorkerStaleCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range wall {
		found = found || row.ID == f.runID
	}
	if !found {
		t.Fatalf("short non-wait budget did not request fixture lead: %v", wall)
	}

	sweep := func(want int) {
		t.Helper()
		ids, err := f.q.SupersedeExitedPlanCrossChecks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		for _, id := range ids {
			if id == f.runID {
				got++
			}
		}
		if got != want {
			t.Fatalf("sweep: fixture count=%d, want %d (all ids=%v)", got, want, ids)
		}
	}
	sweep(0) // the earlier requeue already settled this check
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'queued' WHERE id = $1`, f.runID)
	assertPlanCrossCheckSettled(ctx, t, f, childID, 110)
	sweep(0) // synchronous settlement leaves no work for the fallback
	var verdict, reason, childStatus string
	var credit int32
	if err := f.pool.QueryRow(ctx, `SELECT cc.verdict, cc.reason_class, child.status,
        lead.budget_paused_seconds FROM cross_checks cc
        JOIN runs lead ON lead.id = cc.lead_run_id
        JOIN runs child ON child.id = cc.checker_run_id WHERE lead.id = $1`, f.runID).
		Scan(&verdict, &reason, &childStatus, &credit); err != nil {
		t.Fatal(err)
	}
	if verdict != "failed" || reason != "superseded" || childStatus != "cancelled" || credit < 110 {
		t.Fatalf("exit: verdict=%s reason=%s child=%s credit=%d", verdict, reason, childStatus, credit)
	}
	if credit != firstCredit {
		t.Fatalf("duplicate wait credit: first=%d second=%d", firstCredit, credit)
	}
	sweep(0)
	var again int32
	if err := f.pool.QueryRow(ctx, `SELECT budget_paused_seconds FROM runs WHERE id = $1`, f.runID).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if again != credit {
		t.Fatalf("duplicate wait credit: first=%d second=%d", credit, again)
	}
}
