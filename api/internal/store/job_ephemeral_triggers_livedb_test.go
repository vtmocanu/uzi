package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1908 (D-A): the two ephemeral trigger queries gain a job arm. A queued kind='job' run
// carries required_capabilities '{}', so the ordinary capability-gap test can never fire for it;
// instead a job is placeable ONLY on an online, non-draining, non-ephemeral, NON-docker worker
// advertising the 'job_runner_v1' protocol capability (ClaimRun's non-bypassable job clause).
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh.

const jobRunnerCap = "job_runner_v1"

// jobWorker inserts an online worker advertising the given protocol capabilities.
func (fx *fleetFixture) jobWorker(name string, cap *int32, docker bool, protocolCaps ...string) uuid.UUID {
	fx.t.Helper()
	id := fx.worker(name, cap, docker)
	if protocolCaps == nil {
		protocolCaps = []string{}
	}
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, id, protocolCaps)
	return id
}

// queuedJob inserts a queued, unowned, repo-less job run whose status_since is old enough to
// clear any saturation debounce.
func (fx *fleetFixture) queuedJob() uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, auto_approve, required_capabilities, status, status_since)
		 VALUES ($1, $2, 'job', 'research', 'job', 'prompt', true, '{}', 'queued', now() - interval '1 hour')`,
		id, fx.userID)
	return id
}

func saturationRunIDs(fx *fleetFixture) map[uuid.UUID]bool {
	fx.t.Helper()
	rows, err := fx.q.ListSaturationQueuedRunsForEphemeral(fx.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
		SaturationDelay: pgtype.Interval{Valid: true},
		MaxRows:         1000,
		MaxPerUser:      1000,
	})
	if err != nil {
		fx.t.Fatalf("ListSaturationQueuedRunsForEphemeral: %v", err)
	}
	out := make(map[uuid.UUID]bool, len(rows))
	for _, r := range rows {
		if r.UserID == fx.userID {
			out[r.ID] = true
		}
	}
	return out
}

// TestJobGapTriggerLiveDB: the gap trigger fires for a job when NO online worker can claim it
// (an old-image non-docker worker, or a docker worker even one advertising the capability) and
// stays quiet when a job-capable worker is online.
//
// CALIBRATION (gap arm): remove the job-specific `AND (r.kind <> 'job' OR NOT EXISTS ...)` block
// in ListUnplaceableQueuedRunsForEphemeral and regenerate; the "old-image" and "docker" subtests
// then fail, because the ordinary capability test skips a zero-cap job.
func TestJobGapTriggerLiveDB(t *testing.T) {
	t.Run("only an old-image non-docker worker is online: fires", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		fx.jobWorker("old-image", capOf(1), false) // no job_runner_v1
		runID := fx.queuedJob()
		if got := unplaceableRunIDs(fx); !got[runID] {
			t.Fatalf("job %s not surfaced although the only online worker cannot claim a job", runID)
		}
	})

	t.Run("only a docker worker is online, even advertising the capability: fires", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		fx.jobWorker("docker", capOf(1), true, jobRunnerCap)
		runID := fx.queuedJob()
		if got := unplaceableRunIDs(fx); !got[runID] {
			t.Fatalf("job %s not surfaced although a docker worker never claims a job", runID)
		}
	})

	t.Run("no worker at all: fires", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		runID := fx.queuedJob()
		if got := unplaceableRunIDs(fx); !got[runID] {
			t.Fatalf("job %s not surfaced with no worker online", runID)
		}
	})

	t.Run("a job-capable non-docker worker is online: does not fire", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		fx.jobWorker("old-image", capOf(1), false)
		fx.jobWorker("capable", capOf(1), false, jobRunnerCap)
		runID := fx.queuedJob()
		if got := unplaceableRunIDs(fx); got[runID] {
			t.Fatalf("job %s surfaced although a job-capable worker is online", runID)
		}
	})

	t.Run("a draining job-capable worker does not count: fires", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		w := fx.jobWorker("capable", capOf(1), false, jobRunnerCap)
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET draining_since = now() WHERE id = $1`, w)
		runID := fx.queuedJob()
		if got := unplaceableRunIDs(fx); !got[runID] {
			t.Fatalf("job %s not surfaced; a draining worker claims nothing", runID)
		}
	})

	t.Run("a failed job is never surfaced, so a failed job cannot re-provision", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		runID := fx.queuedJob()
		if got := unplaceableRunIDs(fx); !got[runID] {
			t.Fatalf("precondition: queued job %s must be surfaced", runID)
		}
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'failed', fail_origin = 'no_job_capable_worker', finished_at = now() WHERE id = $1`, runID)
		if got := unplaceableRunIDs(fx); got[runID] {
			t.Fatalf("failed job %s still surfaced by the gap trigger", runID)
		}
	})

	t.Run("a job of a user who has not opted in is never surfaced", func(t *testing.T) {
		fx := newFleetFixture(t) // NOT opted in
		runID := fx.queuedJob()
		if got := unplaceableRunIDs(fx); got[runID] {
			t.Fatalf("job %s surfaced for a user who did not opt in", runID)
		}
	})
}

// TestJobSaturationTriggerLiveDB: the saturation trigger fires for a job only when at least one
// job-capable (non-docker, job_runner_v1) worker exists and EVERY such worker is at its cap. An
// old-image or docker worker, free or busy, changes nothing.
//
// CALIBRATION (saturation arm): remove the `AND (r.kind <> 'job' OR (...))` arm from the
// free-slot NOT EXISTS of ListSaturationQueuedRunsForEphemeral and regenerate; the "old-image
// worker is free" subtest then fails, because the free old worker counts as capable-with-room.
func TestJobSaturationTriggerLiveDB(t *testing.T) {
	t.Run("every job-capable worker is at its cap: fires", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		w := fx.jobWorker("capable", capOf(1), false, jobRunnerCap)
		fx.holdActive(w, 1)
		runID := fx.queuedJob()
		if got := saturationRunIDs(fx); !got[runID] {
			t.Fatalf("job %s not surfaced although the only job-capable worker is full", runID)
		}
	})

	t.Run("a job-capable worker has a free slot: does not fire", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		full := fx.jobWorker("capable-full", capOf(1), false, jobRunnerCap)
		fx.holdActive(full, 1)
		fx.jobWorker("capable-free", capOf(2), false, jobRunnerCap)
		runID := fx.queuedJob()
		if got := saturationRunIDs(fx); got[runID] {
			t.Fatalf("job %s surfaced although a job-capable worker has a free slot", runID)
		}
	})

	t.Run("an unbounded job-capable worker always has room: does not fire", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		fx.jobWorker("capable-unbounded", nil, false, jobRunnerCap)
		runID := fx.queuedJob()
		if got := saturationRunIDs(fx); got[runID] {
			t.Fatalf("job %s surfaced although a NULL-cap job-capable worker has room", runID)
		}
	})

	t.Run("old-image worker is free while the capable worker is full: fires", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		full := fx.jobWorker("capable-full", capOf(1), false, jobRunnerCap)
		fx.holdActive(full, 1)
		fx.jobWorker("old-image-free", capOf(4), false) // free, but cannot claim a job
		runID := fx.queuedJob()
		if got := saturationRunIDs(fx); !got[runID] {
			t.Fatalf("job %s not surfaced; a free old-image worker cannot claim a job and must not count as room", runID)
		}
	})

	t.Run("docker worker is free while the capable worker is full: fires", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		full := fx.jobWorker("capable-full", capOf(1), false, jobRunnerCap)
		fx.holdActive(full, 1)
		fx.jobWorker("docker-free", capOf(4), true, jobRunnerCap) // free, but a docker worker never claims a job
		runID := fx.queuedJob()
		if got := saturationRunIDs(fx); !got[runID] {
			t.Fatalf("job %s not surfaced; a free docker worker cannot claim a job and must not count as room", runID)
		}
	})

	t.Run("no job-capable worker exists (only old-image or docker): does not fire", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		old := fx.jobWorker("old-image", capOf(1), false)
		fx.holdActive(old, 1)
		dk := fx.jobWorker("docker", capOf(1), true, jobRunnerCap)
		fx.holdActive(dk, 1)
		runID := fx.queuedJob()
		if got := saturationRunIDs(fx); got[runID] {
			t.Fatalf("job %s surfaced by the saturation trigger with no capable worker; that is the gap trigger's case", runID)
		}
	})

	t.Run("a zero-capability non-job run keeps its old saturation behaviour", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		w := fx.jobWorker("plain", capOf(1), false) // old-image worker, full
		fx.holdActive(w, 1)
		runID := fx.queuedRun()
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, runID)
		if got := saturationRunIDs(fx); !got[runID] {
			t.Fatalf("issue run %s not surfaced; the job arm must not change non-job saturation", runID)
		}
	})
}
