package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// Issue #1994, live-DB half: terminal_pending_since is the first-seen time of a pending
// snapshot entry, and the health ladder flags a run whose journaled outcome stays undelivered.
// Skipped unless UZI_TEST_DATABASE_URL is set (setupCodexLiveDB skips).

// readPendingSince returns the persisted terminal_pending_since and terminal_pending_until of
// (worker, run); since.Valid is false for NULL.
func readPendingSince(t *testing.T, e codexTestEnv, workerID, runID uuid.UUID) (since, until pgtype.Timestamptz) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx,
		`SELECT terminal_pending_since, terminal_pending_until FROM worker_active_runs WHERE worker_id = $1 AND run_id = $2`,
		workerID, runID).Scan(&since, &until); err != nil {
		t.Fatalf("read terminal_pending_since: %v", err)
	}
	return since, until
}

// heartbeatSnapshot applies one heartbeat-mode snapshot at the next epoch listing the entries.
func heartbeatSnapshot(t *testing.T, svc *Service, e codexTestEnv, workerID uuid.UUID, entries ...ActiveRunEntry) {
	t.Helper()
	snap := &ActiveSnapshot{
		SnapshotEpoch: workerEpoch(t, e, workerID) + 1,
		RegisterNonce: "n1",
		Active:        entries,
	}
	applied, err := applySnapshot(t, svc, e, workerID, snap, snapshotModeHeartbeat)
	if err != nil || !applied {
		t.Fatalf("heartbeat snapshot applied=%v err=%v", applied, err)
	}
}

// runHealthOf reads the persisted health flag and reason of a run.
func runHealthOf(t *testing.T, e codexTestEnv, runID uuid.UUID) (string, string) {
	t.Helper()
	var health string
	var reason pgtype.Text
	if err := e.pool.QueryRow(e.ctx, `SELECT health, health_reason FROM runs WHERE id = $1`, runID).Scan(&health, &reason); err != nil {
		t.Fatalf("read run health: %v", err)
	}
	return health, reason.String
}

// healthSvcLive builds a Service over the live store with health detection enabled.
func healthSvcLive(e codexTestEnv) *Service {
	svc := snapshotSvc(e, testParams())
	svc.healthSettings = defaultHealthSettings()
	return svc
}

func TestPendingSinceStableAcrossRenewalsAndFlagsLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	svc := healthSvcLive(e)
	o := seedReevalOwner(t, e, BindModeAuto, false)
	workerID := o.workerID
	e.exec(`UPDATE workers SET snapshot_register_nonce = 'n1', snapshot_epoch = 0, last_heartbeat_at = now() WHERE id = $1`, workerID)
	runID := seedRunningRun(t, e, o, nextOutageIID(), 0)
	setRunGeneration(t, e, runID, 5)

	heartbeatSnapshot(t, svc, e, workerID, entry(runID, 5, "running", true))
	since1, until1 := readPendingSince(t, e, workerID, runID)
	if !since1.Valid || !until1.Valid {
		t.Fatalf("pending row must carry since and until, got since=%v until=%v", since1, until1)
	}

	// A fresh pending entry (age < threshold) is not flagged with the new reason.
	svc.detectRunHealth(e.ctx, time.Now().Add(svc.pendingOutcomeThreshold()/2))
	if _, reason := runHealthOf(t, e, runID); reason == reasonOutcomeUndelivered {
		t.Fatalf("a fresh pending entry must not raise %q", reasonOutcomeUndelivered)
	}

	// Repeated renewals keep since constant while the lease renews.
	for i := 0; i < 3; i++ {
		heartbeatSnapshot(t, svc, e, workerID, entry(runID, 5, "running", true))
	}
	since2, until2 := readPendingSince(t, e, workerID, runID)
	if !since2.Time.Equal(since1.Time) {
		t.Fatalf("since moved across renewals: %v -> %v", since1.Time, since2.Time)
	}
	if !until2.Time.After(until1.Time) {
		t.Fatalf("lease did not renew: %v -> %v", until1.Time, until2.Time)
	}

	// Past the threshold the run is flagged stalled with the undelivered reason.
	svc.detectRunHealth(e.ctx, time.Now().Add(svc.pendingOutcomeThreshold()+time.Minute))
	health, reason := runHealthOf(t, e, runID)
	if health != healthStalled || reason != reasonOutcomeUndelivered {
		t.Fatalf("health=%q reason=%q, want %q / %q", health, reason, healthStalled, reasonOutcomeUndelivered)
	}
}

func TestPendingSinceResetsLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	svc := healthSvcLive(e)
	o := seedReevalOwner(t, e, BindModeAuto, false)
	workerID := o.workerID
	e.exec(`UPDATE workers SET snapshot_register_nonce = 'n1', snapshot_epoch = 0, last_heartbeat_at = now() WHERE id = $1`, workerID)
	runID := seedRunningRun(t, e, o, nextOutageIID(), 0)
	setRunGeneration(t, e, runID, 5)

	heartbeatSnapshot(t, svc, e, workerID, entry(runID, 5, "running", true))
	first, _ := readPendingSince(t, e, workerID, runID)

	// Listed live: since clears; re-listed pending: a NEW first-seen time.
	heartbeatSnapshot(t, svc, e, workerID, entry(runID, 5, "running", false))
	if s, _ := readPendingSince(t, e, workerID, runID); s.Valid {
		t.Fatalf("a live entry must have NULL since, got %v", s.Time)
	}
	heartbeatSnapshot(t, svc, e, workerID, entry(runID, 5, "running", true))
	second, _ := readPendingSince(t, e, workerID, runID)
	if !second.Valid || !second.Time.After(first.Time) {
		t.Fatalf("since must restart after a live listing: first=%v second=%v", first.Time, second.Time)
	}

	// Omitted from a heartbeat snapshot (row deleted), then re-listed pending: restarts again.
	heartbeatSnapshot(t, svc, e, workerID)
	if n := countActiveRuns(t, e, workerID); n != 0 {
		t.Fatalf("omitted run must be gone, rows=%d", n)
	}
	heartbeatSnapshot(t, svc, e, workerID, entry(runID, 5, "running", true))
	third, _ := readPendingSince(t, e, workerID, runID)
	if !third.Time.After(second.Time) {
		t.Fatalf("since must restart after omission: second=%v third=%v", second.Time, third.Time)
	}

	// Generation change restarts it.
	setRunGeneration(t, e, runID, 6)
	heartbeatSnapshot(t, svc, e, workerID, entry(runID, 6, "running", true))
	fourth, _ := readPendingSince(t, e, workerID, runID)
	if !fourth.Time.After(third.Time) {
		t.Fatalf("since must restart on a generation change: third=%v fourth=%v", third.Time, fourth.Time)
	}
}

func TestPendingSincePreservedByRegisterLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	svc := healthSvcLive(e)
	o := seedReevalOwner(t, e, BindModeAuto, false)
	workerID := o.workerID
	e.exec(`UPDATE workers SET snapshot_register_nonce = 'n1', snapshot_epoch = 0, last_heartbeat_at = now() WHERE id = $1`, workerID)
	runID := seedRunningRun(t, e, o, nextOutageIID(), 0)
	setRunGeneration(t, e, runID, 5)

	heartbeatSnapshot(t, svc, e, workerID, entry(runID, 5, "running", true))
	first, _ := readPendingSince(t, e, workerID, runID)

	applied, err := applySnapshot(t, svc, e, workerID, &ActiveSnapshot{
		Active: []ActiveRunEntry{entry(runID, 5, "running", true)},
	}, snapshotModeRegister)
	if err != nil || !applied {
		t.Fatalf("register snapshot applied=%v err=%v", applied, err)
	}
	if after, _ := readPendingSince(t, e, workerID, runID); !after.Time.Equal(first.Time) {
		t.Fatalf("register must preserve since: %v -> %v", first.Time, after.Time)
	}
}

// TestPendingOutcomeHealthRequiresCurrentOwnerGenerationLeaseLiveDB: a pending row that belongs
// to a different worker than runs.worker_id, sits at another generation, or has an expired lease
// must not raise the undelivered reason however old its since is. The positive control proves
// the same seed shape does flag when owner, generation and lease all match.
func TestPendingOutcomeHealthRequiresCurrentOwnerGenerationLeaseLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	svc := healthSvcLive(e)
	o := seedReevalOwner(t, e, BindModeAuto, false)
	otherWorker := seedOutageWorker(t, e, o.userID, 0)

	const insert = `INSERT INTO worker_active_runs
	    (worker_id, run_id, claim_generation, phase, terminal_pending, terminal_pending_until, terminal_pending_since, snapshot_epoch, reported_at)
	    VALUES ($1, $2, $3, 'running', true, now() + $4::interval, now() - interval '1 hour', 1, now())`

	cases := []struct {
		name     string
		worker   uuid.UUID
		gen      int64
		lease    string
		wantFlag bool
	}{
		{"wrong worker", otherWorker, 5, "1 hour", false},
		{"wrong generation", o.workerID, 4, "1 hour", false},
		{"expired lease", o.workerID, 5, "-1 minute", false},
		{"positive control", o.workerID, 5, "1 hour", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := seedRunningRun(t, e, o, nextOutageIID(), 0)
			setRunGeneration(t, e, runID, 5)
			e.exec(insert, tc.worker, runID, tc.gen, tc.lease)
			svc.detectRunHealth(e.ctx, time.Now().Add(time.Minute))
			_, reason := runHealthOf(t, e, runID)
			if got := reason == reasonOutcomeUndelivered; got != tc.wantFlag {
				t.Fatalf("reason=%q flagged=%v want %v", reason, got, tc.wantFlag)
			}
		})
	}
}
