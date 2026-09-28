package workersvc

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1809 M6 (D8) checkpoint durability report, against a REAL Postgres: an APPLIED park stores
// the report's checkpoint_contains_latest (the returned run carries it), a later park without the
// flag clears it to NULL, a resume does not touch it, and a refused park writes nothing. Custody is
// untouched by the flag: a false report on a disk park leaves the generation's hold open.
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

	// (2) A resume does not clear it (surfaces show it only while parked).
	gen := e.dpResume(t, runID, w)
	if v := e.cdlValue(t, runID); !v.Valid || v.Bool {
		t.Fatalf("after resume checkpoint_contains_latest = %+v, want the last park's false kept", v)
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
