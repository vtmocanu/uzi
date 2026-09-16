package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1390 M2c — the reported-active-runs overlay: a worker with worker_active_runs rows gets
// its snapshot surfaced on WorkerDTO.reported_runs (run_id/phase/generation), and a worker with
// none gets a non-nil [] that marshals as a JSON array, not null. This exercises the batched
// read (reportedRunsByWorker) plus the overlay (overlayReportedRuns) that the two list endpoints
// and PatchWorker share — including the never-null contract the api-contract fixture and the TS
// type both pin.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package (…LiveDB) so this auto-runs in CI's test-api-store-it job
// with no workflow edit.
func TestReportedRunsOverlayLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	q := store.New(pool)
	box := newHandlerTestBox(t)
	h := &Handler{
		pool:      pool,
		q:         q,
		box:       box,
		wsvc:      workersvc.New(q, box, workersvc.Params{}),
		version:   "dev",
		now:       time.Now,
		startedAt: time.Now(),
	}

	owner := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("rr-owner-%s@e2e", uuid.NewString()[:8]))

	// Worker A holds two reported runs; worker B holds none — the two branches of the contract.
	workerA, workerB := uuid.New(), uuid.New()
	seedWorker := func(id uuid.UUID, name string) {
		mustExecT(ctx, t, pool,
			`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
			id, owner, name, id[:])
	}
	seedWorker(workerA, "rr-worker-a")
	seedWorker(workerB, "rr-worker-b")

	// Two chat runs owned by worker A satisfy the worker_active_runs.run_id FK. The run KIND is
	// irrelevant to the overlay — reportedRunsByWorker reads worker_active_runs directly — so a
	// chat run (no repo/connection needed) keeps this test on the read + overlay under test.
	run1, run2 := uuid.New(), uuid.New()
	seedRun := func(id uuid.UUID, gen int64) {
		mustExecT(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation)
			 VALUES ($1, $2, NULL, 'chat', NULL, 't', 'd', 'running', $3, $4)`,
			id, owner, workerA, gen)
	}
	seedRun(run1, 5)
	seedRun(run2, 6)

	// One live entry and one held (awaiting_approval) entry for worker A; none for B.
	insertSnapshot := func(runID uuid.UUID, phase string, gen int64) {
		mustExecT(ctx, t, pool,
			`INSERT INTO worker_active_runs (worker_id, run_id, claim_generation, phase, terminal_pending, snapshot_epoch, reported_at)
			 VALUES ($1, $2, $3, $4, false, 1, now())`,
			workerA, runID, gen, phase)
	}
	insertSnapshot(run1, "running", 5)
	insertSnapshot(run2, "awaiting_approval", 6)

	byWorker := h.reportedRunsByWorker(ctx, []uuid.UUID{workerA, workerB})

	// Worker A: both entries, mapped run_id/phase/generation (asserted by id, order-independent).
	gotA := byWorker[workerA]
	if len(gotA) != 2 {
		t.Fatalf("worker A reported_runs = %d entries, want 2: %+v", len(gotA), gotA)
	}
	byRun := map[string]apitypes.WorkerReportedRunDTO{}
	for _, rr := range gotA {
		byRun[rr.RunID] = rr
	}
	if rr := byRun[run1.String()]; rr.Phase != "running" || rr.ClaimGeneration != 5 {
		t.Errorf("run1 entry = %+v, want phase=running gen=5", rr)
	}
	if rr := byRun[run2.String()]; rr.Phase != "awaiting_approval" || rr.ClaimGeneration != 6 {
		t.Errorf("run2 entry = %+v, want phase=awaiting_approval gen=6", rr)
	}

	// Worker B: a pre-seeded, non-nil empty slice — never absent, never nil.
	gotB, ok := byWorker[workerB]
	if !ok {
		t.Fatalf("worker B missing from the batched map (every listed worker must be pre-seeded to [])")
	}
	if gotB == nil {
		t.Fatalf("worker B reported_runs is nil, want a non-nil empty slice")
	}
	if len(gotB) != 0 {
		t.Fatalf("worker B reported_runs = %d entries, want 0: %+v", len(gotB), gotB)
	}

	// The overlay attaches the slice, and a worker with none marshals reported_runs as [] not
	// null — the never-null wire contract. Also verify worker A's DTO overlay carries both.
	var dtoB apitypes.WorkerDTO
	h.overlayReportedRuns(&dtoB, byWorker, workerB)
	bb, err := json.Marshal(dtoB)
	if err != nil {
		t.Fatalf("marshal worker B dto: %v", err)
	}
	if !strings.Contains(string(bb), `"reported_runs":[]`) {
		t.Errorf("worker B dto reported_runs did not marshal as []: %s", bb)
	}
	if strings.Contains(string(bb), `"reported_runs":null`) {
		t.Errorf("worker B dto reported_runs marshaled as null, want []: %s", bb)
	}

	var dtoA apitypes.WorkerDTO
	h.overlayReportedRuns(&dtoA, byWorker, workerA)
	if len(dtoA.ReportedRuns) != 2 {
		t.Errorf("worker A dto reported_runs = %d entries, want 2 after overlay", len(dtoA.ReportedRuns))
	}
}
