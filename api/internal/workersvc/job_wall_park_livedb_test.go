package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestJobWallParkReportRefusedLiveDB pins PRD #1908 D-E on SetRunWallPark: a running job past its
// wall that reports wall_park changes no row. It stays running with its worker, and the wall
// pause input stays unconsumed (a job's wall limit fails it through FailJobsPastWallDeadline, it
// never parks). Driven through the real ReportWallPark service path. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway database.
//
// MUTATION CHECK: removing the kind <> 'job' conjunct from the UPDATE in SetRunWallPark parks the
// job paused/budget_exhausted; removing it from the consumed_wall CTE consumes the wall input.
func TestJobWallParkReportRefusedLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	owner := e.seedJobUser(t)
	workerID := e.seedWorkerRow(t, owner, false, nil, jobCap)
	// Started two hours ago against a 60 s wall: well past the deadline, claim not released.
	job := e.seedRawJob(t, owner, "running", &workerID, 2*time.Hour, 60)
	e.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'pause', 'wall')`, job)
	wkr := store.Worker{ID: workerID, UserID: owner, Name: "w", Status: "online", ProtocolCapabilities: []string{jobCap}}
	gen := int64(1)

	run, applied, err := e.svc.ReportWallPark(e.ctx, wkr, job, "deadbeef", true, &gen, nil)
	if err != nil || applied {
		t.Fatalf("ReportWallPark = applied %v, err %v; want a refused (applied=false) report", applied, err)
	}
	if run.Status != "running" {
		t.Fatalf("answered run status = %q, want running", run.Status)
	}
	var status string
	var hold *string
	var worker *uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT status, hold_reason, worker_id FROM runs WHERE id = $1`, job).Scan(&status, &hold, &worker); err != nil {
		t.Fatal(err)
	}
	if status != "running" || hold != nil || worker == nil {
		t.Fatalf("job row = status %q, hold %v, worker %v; want running, no hold, worker kept", status, hold, worker)
	}
	var unconsumed int
	if err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'pause' AND body = 'wall' AND consumed_at IS NULL AND applied_at IS NULL`,
		job).Scan(&unconsumed); err != nil {
		t.Fatal(err)
	}
	if unconsumed != 1 {
		t.Fatalf("unconsumed wall inputs = %d, want 1 (a refused park must not consume it)", unconsumed)
	}
}
