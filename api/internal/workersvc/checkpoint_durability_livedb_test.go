package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1809 M6 (D8) checkpoint durability report, against a REAL Postgres: an APPLIED park stores
// the report's checkpoint_contains_latest (the returned run carries it), a later park without the
// flag clears it to NULL, and a refused park writes nothing. Custody is untouched by the flag: a
// false report on a disk park leaves the generation's hold open. A real resume (ClaimRun, or a
// running report through SetRunRunning) clears it, so a later server-side park that carries no
// report reads NULL instead of the older park's value.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh).

func (e interlockLiveDB) cdlValue(t *testing.T, runID uuid.UUID) pgtype.Bool {
	t.Helper()
	var v pgtype.Bool
	if err := e.pool.QueryRow(e.ctx, `SELECT checkpoint_contains_latest FROM runs WHERE id = $1`, runID).Scan(&v); err != nil {
		t.Fatalf("read checkpoint_contains_latest: %v", err)
	}
	return v
}

func TestCheckpointDurabilityOnParksLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.diskParkService(t, 3)

	w := e.seedWorker(t, nil)
	wkr := store.Worker{ID: w}
	runID := e.fpSeedRunning(t, w, 1)
	hold := mhOpenHold(t, e, runID, 1, w)

	// (1) A data_volume_full park reporting the checkpoint does NOT contain the latest work.
	report := dataVolumeFullReport(1, boolPtr(true))
	report.CheckpointContainsLatest = boolPtr(false)
	run, applied, err := svc.SetState(e.ctx, wkr, runID, report)
	if err != nil || !applied || run.Status != "recovery_wait" {
		t.Fatalf("disk park: applied=%v status=%q err=%v, want applied recovery_wait", applied, run.Status, err)
	}
	if !run.CheckpointContainsLatest.Valid || run.CheckpointContainsLatest.Bool {
		t.Fatalf("returned run checkpoint_contains_latest = %+v, want false", run.CheckpointContainsLatest)
	}
	if v := e.cdlValue(t, runID); !v.Valid || v.Bool {
		t.Fatalf("stored checkpoint_contains_latest = %+v, want false", v)
	}
	// Custody logic unchanged: the false report keeps the generation's hold open.
	if st := mhHoldState(t, e, hold); st != "open" {
		t.Fatalf("hold state after a checkpoint_contains_latest=false park = %q, want open; custody must not key on the flag", st)
	}

	// (2) dpResume re-claims with raw SQL (not ClaimRun / SetRunRunning, which clear the flag;
	// TestCheckpointDurabilityClearedOnResumeLiveDB covers those), so the false value is still
	// stored when the next park lands and step (3) proves the park's own clearing write.
	gen := e.dpResume(t, runID, w)
	if v := e.cdlValue(t, runID); !v.Valid || v.Bool {
		t.Fatalf("after the raw re-claim checkpoint_contains_latest = %+v, want false still stored", v)
	}

	// (3) An ordinary recovery park WITHOUT the flag clears the older park's value.
	run, applied, err = svc.SetState(e.ctx, wkr, runID, StateRequest{State: "recovery_wait", ClaimGeneration: i64Ptr(gen)})
	if err != nil || !applied || run.Status != "recovery_wait" {
		t.Fatalf("plain recovery park: applied=%v status=%q err=%v", applied, run.Status, err)
	}
	if v := e.cdlValue(t, runID); v.Valid {
		t.Fatalf("a park without the flag left checkpoint_contains_latest = %+v, want NULL", v)
	}
	if run.CheckpointContainsLatest.Valid {
		t.Fatalf("returned run carries %+v after a flagless park, want NULL", run.CheckpointContainsLatest)
	}

	// (4) An owner pause park reporting the checkpoint contains the latest work.
	gen = e.dpResume(t, runID, w)
	e.exec(t, `UPDATE runs SET pause_requested_at = now(), pause_mode = 'now' WHERE id = $1`, runID)
	run, applied, err = svc.SetState(e.ctx, wkr, runID, StateRequest{State: "paused", ClaimGeneration: i64Ptr(gen), CheckpointContainsLatest: boolPtr(true)})
	if err != nil || !applied || run.Status != "paused" {
		t.Fatalf("pause park: applied=%v status=%q err=%v, want applied paused", applied, run.Status, err)
	}
	if v := e.cdlValue(t, runID); !v.Valid || !v.Bool {
		t.Fatalf("stored checkpoint_contains_latest = %+v, want true", v)
	}

	// (5) A REFUSED park (a redelivery onto the already-paused run) writes nothing.
	_, applied, err = svc.SetState(e.ctx, wkr, runID, StateRequest{State: "paused", ClaimGeneration: i64Ptr(gen), CheckpointContainsLatest: boolPtr(false)})
	if applied {
		t.Fatalf("a redelivered pause park applied (err=%v); it must be refused", err)
	}
	if v := e.cdlValue(t, runID); !v.Valid || !v.Bool {
		t.Fatalf("a refused park changed checkpoint_contains_latest to %+v, want true kept", v)
	}
}

// TestCheckpointDurabilityClearedOnResumeLiveDB (PRD #1809 M6, D8, review N1): the flag describes
// only the park that reported it. A worker park reports false; the run is resumed through the REAL
// claim (ClaimRun), which clears it; a server-side park of the claimed run (credential_disabled,
// which never carries a report) then reads NULL, not the older park's false. A running report
// (SetRunRunning, the in-place and post-claim resume) clears it the same way.
func TestCheckpointDurabilityClearedOnResumeLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.diskParkService(t, 3)

	w := e.seedWorker(t, nil)
	e.exec(t, `UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, w)
	wkr, err := e.q.GetWorkerByID(e.ctx, w)
	if err != nil {
		t.Fatalf("read worker: %v", err)
	}

	// (a) A worker park reporting the checkpoint does NOT contain the latest work.
	runID := e.fpSeedRunning(t, w, 1)
	report := dataVolumeFullReport(1, boolPtr(true))
	report.CheckpointContainsLatest = boolPtr(false)
	if run, applied, err := svc.SetState(e.ctx, wkr, runID, report); err != nil || !applied || run.Status != "recovery_wait" {
		t.Fatalf("disk park: applied=%v status=%q err=%v, want applied recovery_wait", applied, run.Status, err)
	}
	if v := e.cdlValue(t, runID); !v.Valid || v.Bool {
		t.Fatalf("stored checkpoint_contains_latest = %+v, want false", v)
	}

	// (b) Resume: the retry clock promotes it to queued, and the real claim clears the flag.
	e.exec(t, `UPDATE runs SET recovery_retry_not_before = now() - interval '1 minute' WHERE id = $1`, runID)
	if _, err := e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now())); err != nil {
		t.Fatalf("PromoteRecoveryWaitRuns: %v", err)
	}
	claimed, err := e.q.ClaimRun(e.ctx, claimRunParams(wkr))
	if err != nil || claimed.ID != runID || claimed.Status != "claimed" {
		t.Fatalf("ClaimRun = %s/%q err=%v, want %s claimed", claimed.ID, claimed.Status, err, runID)
	}
	if claimed.CheckpointContainsLatest.Valid {
		t.Fatalf("ClaimRun returned checkpoint_contains_latest = %+v, want NULL", claimed.CheckpointContainsLatest)
	}

	// (c) A server-side park of the claimed run carries no report and reads NULL.
	n, err := e.q.ParkCredentialDisabledRun(e.ctx, store.ParkCredentialDisabledRunParams{
		ID: runID, WorkerID: pgconv.UUID(w), ClaimGeneration: claimed.ClaimGeneration,
	})
	if err != nil || n != 1 {
		t.Fatalf("ParkCredentialDisabledRun = %d err=%v, want 1 row", n, err)
	}
	if status, _, _, _, _ := e.dpRun(t, runID); status != "paused" {
		t.Fatalf("status after the credential park = %q, want paused", status)
	}
	if v := e.cdlValue(t, runID); v.Valid {
		t.Fatalf("server-side park shows checkpoint_contains_latest = %+v, want NULL (the older park's false must not reappear)", v)
	}

	// (d) A running report clears it too: a second run parks false, is re-claimed with raw SQL (so
	// the value survives to the report), and its first running report clears it.
	run2 := e.fpSeedRunning(t, w, 1)
	report = dataVolumeFullReport(1, boolPtr(true))
	report.CheckpointContainsLatest = boolPtr(false)
	if _, applied, err := svc.SetState(e.ctx, wkr, run2, report); err != nil || !applied {
		t.Fatalf("disk park run2: applied=%v err=%v", applied, err)
	}
	gen := e.dpResume(t, run2, w)
	if v := e.cdlValue(t, run2); !v.Valid || v.Bool {
		t.Fatalf("run2 before the running report = %+v, want false still stored", v)
	}
	run, applied, err := svc.SetState(e.ctx, wkr, run2, StateRequest{State: "running", ClaimGeneration: i64Ptr(gen)})
	if err != nil || !applied || run.Status != "running" {
		t.Fatalf("running report: applied=%v status=%q err=%v", applied, run.Status, err)
	}
	if v := e.cdlValue(t, run2); v.Valid {
		t.Fatalf("after the running report checkpoint_contains_latest = %+v, want NULL", v)
	}
}
