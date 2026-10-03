package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1909 M1, the job_files_v1 ROLLOUT GATE at the store layer. A job created since the file
// protocol landed carries runs.job_protocol = 2; every job clause that reads 'job_runner_v1' also
// requires 'job_files_v1' for such a job, while a job created before the stamp existed
// (job_protocol NULL) stays served by any job_runner_v1 worker. These tests drive each MIRRORED
// query (the ephemeral gap and saturation triggers and the per-run claimable count) plus the
// column's own CHECK. ClaimRun, its peer arm and LockUnservableEphemeralJobWorkers live in
// workersvc/job_files_gate_livedb_test.go. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres; run via e2e/run-store-it.sh.
//
// CALIBRATION (one per query): delete the `AND (r.job_protocol IS NULL OR 'job_files_v1' = ANY(...))`
// arm from the named query, regenerate, and the named subtest below turns red.

const jobFilesCap = "job_files_v1"

// queuedNewJob inserts a queued, unowned job stamped with the file protocol.
func (fx *fleetFixture) queuedNewJob() uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, auto_approve, required_capabilities, status, status_since, job_protocol)
		 VALUES ($1, $2, 'job', 'research', 'job', 'prompt', true, '{}', 'queued', now() - interval '1 hour', 2)`,
		id, fx.userID)
	return id
}

// TestJobFilesGateGapTriggerLiveDB (ListUnplaceableQueuedRunsForEphemeral, the job arm): a new job
// is unplaceable on a job_runner_v1-only fleet and placeable once a job_files_v1 worker exists; a
// pre-change job stays placeable by the job_runner_v1-only worker.
func TestJobFilesGateGapTriggerLiveDB(t *testing.T) {
	t.Run("new job, only a job_runner_v1 worker: fires", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		fx.jobWorker("runner-only", capOf(1), false, jobRunnerCap)
		runID := fx.queuedNewJob()
		if got := unplaceableRunIDs(fx); !got[runID] {
			t.Fatalf("new job %s not surfaced although the only worker lacks job_files_v1", runID)
		}
	})
	t.Run("new job, a job_files_v1 worker is online: does not fire", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		fx.jobWorker("runner-only", capOf(1), false, jobRunnerCap)
		fx.jobWorker("files", capOf(1), false, jobRunnerCap, jobFilesCap)
		runID := fx.queuedNewJob()
		if got := unplaceableRunIDs(fx); got[runID] {
			t.Fatalf("new job %s surfaced although a job_runner_v1+job_files_v1 worker is online", runID)
		}
	})
	t.Run("pre-change job (job_protocol NULL), only a job_runner_v1 worker: does not fire", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		fx.jobWorker("runner-only", capOf(1), false, jobRunnerCap)
		runID := fx.queuedJob()
		if got := unplaceableRunIDs(fx); got[runID] {
			t.Fatalf("pre-change job %s surfaced although a job_runner_v1 worker can still serve it", runID)
		}
	})
}

// TestJobFilesGateSaturationTriggerLiveDB (ListSaturationQueuedRunsForEphemeral, BOTH job arms: the
// capable-worker existence test and the free-slot test): for a new job a free job_runner_v1-only
// worker is not room, and it is not a capable worker either.
func TestJobFilesGateSaturationTriggerLiveDB(t *testing.T) {
	t.Run("free-slot arm: a free job_runner_v1-only worker is not room while the capable worker is full", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		full := fx.jobWorker("files-full", capOf(1), false, jobRunnerCap, jobFilesCap)
		fx.holdActive(full, 1)
		fx.jobWorker("runner-only-free", capOf(4), false, jobRunnerCap)
		runID := fx.queuedNewJob()
		if got := saturationRunIDs(fx); !got[runID] {
			t.Fatalf("new job %s not surfaced; a free worker without job_files_v1 cannot claim it and must not count as room", runID)
		}
	})
	t.Run("capable-worker arm: no job_files_v1 worker exists, so this is the gap trigger's case", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		w := fx.jobWorker("runner-only", capOf(1), false, jobRunnerCap)
		fx.holdActive(w, 1)
		runID := fx.queuedNewJob()
		if got := saturationRunIDs(fx); got[runID] {
			t.Fatalf("new job %s surfaced by the saturation trigger with no job_files_v1 worker anywhere", runID)
		}
	})
	t.Run("pre-change job: a free job_runner_v1-only worker is room, so it does not fire", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		fx.jobWorker("runner-only-free", capOf(4), false, jobRunnerCap)
		runID := fx.queuedJob()
		if got := saturationRunIDs(fx); got[runID] {
			t.Fatalf("pre-change job %s surfaced although a free job_runner_v1 worker can claim it", runID)
		}
	})
}

// TestJobFilesGateCountsLiveDB: CountOnlineWorkersClaimableForRun mirrors the clause per run, and
// CountOnlineWorkersSatisfyingJobRunner (the queued-reason count, which has no run row) takes the
// requirement as a parameter.
func TestJobFilesGateCountsLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	newJob, oldJob := fx.queuedNewJob(), fx.queuedJob()
	fx.jobWorker("runner-only", capOf(1), false, jobRunnerCap)

	claimable := func(run uuid.UUID) int64 {
		t.Helper()
		n, err := fx.q.CountOnlineWorkersClaimableForRun(fx.ctx, store.CountOnlineWorkersClaimableForRunParams{
			RunID:              run,
			HeartbeatCutoff:    pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
			CapabilityAware:    false,
			CodexCuratedModels: []string{},
		})
		if err != nil {
			t.Fatalf("CountOnlineWorkersClaimableForRun: %v", err)
		}
		return n.Claimable
	}
	satisfying := func(requiresFiles bool) int64 {
		t.Helper()
		n, err := fx.q.CountOnlineWorkersSatisfyingJobRunner(fx.ctx, store.CountOnlineWorkersSatisfyingJobRunnerParams{
			UserID: fx.userID, RequiresJobFiles: requiresFiles,
		})
		if err != nil {
			t.Fatalf("CountOnlineWorkersSatisfyingJobRunner: %v", err)
		}
		return n
	}

	if n := claimable(newJob); n != 0 {
		t.Fatalf("claimable workers for a new job = %d, want 0 (job_runner_v1 alone cannot claim it)", n)
	}
	if n := claimable(oldJob); n != 1 {
		t.Fatalf("claimable workers for a pre-change job = %d, want 1 (job_runner_v1 still serves it)", n)
	}
	if n := satisfying(true); n != 0 {
		t.Fatalf("satisfying workers requiring job files = %d, want 0", n)
	}
	if n := satisfying(false); n != 1 {
		t.Fatalf("satisfying workers not requiring job files = %d, want 1", n)
	}
	fx.jobWorker("files", capOf(1), false, jobRunnerCap, jobFilesCap)
	if n := claimable(newJob); n != 1 {
		t.Fatalf("claimable workers for a new job = %d, want 1 once a job_files_v1 worker is online", n)
	}
	if n := satisfying(true); n != 1 {
		t.Fatalf("satisfying workers requiring job files = %d, want 1", n)
	}
}

// TestRunsJobProtocolCheckLiveDB: job_protocol is NULL unless kind = 'job' (and >= 2 when set), so
// no non-job run can carry the stamp and no job can be stamped with a protocol that predates it.
func TestRunsJobProtocolCheckLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	insert := func(kind string, protocol any) error {
		var err error
		if kind == "job" {
			_, err = fx.pool.Exec(fx.ctx,
				`INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, auto_approve, required_capabilities, status, job_protocol)
				 VALUES ($1, $2, 'job', 'research', 'j', 'p', true, '{}', 'queued', $3)`, uuid.New(), fx.userID, protocol)
		} else {
			_, err = fx.pool.Exec(fx.ctx,
				`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, job_protocol)
				 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', $5)`, uuid.New(), fx.userID, fx.repoID, fx.nextIID(), protocol)
		}
		return err
	}
	if err := insert("job", int16(2)); err != nil {
		t.Fatalf("a job stamped with protocol 2 must insert: %v", err)
	}
	if err := insert("job", nil); err != nil {
		t.Fatalf("a job with a NULL protocol (pre-change shape) must insert: %v", err)
	}
	if err := insert("job", int16(1)); err == nil {
		t.Fatal("a job stamped with protocol 1 inserted; the CHECK must require >= 2")
	}
	if err := insert("issue", int16(2)); err == nil {
		t.Fatal("an issue run stamped with a job protocol inserted; the CHECK must restrict it to kind='job'")
	}
	if err := insert("issue", nil); err != nil {
		t.Fatalf("an issue run with a NULL protocol must insert: %v", err)
	}
}
