package workersvc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// job_files_gate_livedb_test.go covers PRD #1909 M1's job_files_v1 ROLLOUT GATE at the service
// layer: ClaimRun (both arms), CreateJobRun's stamp, LockUnservableEphemeralJobWorkers and the
// queued-reason rung. The mirrored placement queries live in
// store/job_files_gate_livedb_test.go. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// database; run via ./e2e/run-store-it.sh.
//
// CALIBRATION (each test): delete the `r.job_protocol IS NULL OR 'job_files_v1' = ANY(...)` arm from
// the named query in store/*.sql.go (or its queries/*.sql source, then regenerate), re-run, and the
// named subtest goes red; restore the arm.

// newStampedJob creates a job through the real create path, so it carries runs.job_protocol.
func (e jobEnv) newStampedJob(t *testing.T, u uuid.UUID) uuid.UUID {
	t.Helper()
	v, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
	if err != nil {
		t.Fatalf("CreateJobRun: %v", err)
	}
	return v.ID
}

// TestCreateJobRunStampsJobProtocolLiveDB: a created job carries job_protocol = JobProtocolFiles;
// a row inserted the pre-change way carries NULL.
func TestCreateJobRunStampsJobProtocolLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	created := mustRun(t, e.codexTestEnv, e.newStampedJob(t, u))
	if !created.JobProtocol.Valid || created.JobProtocol.Int16 != capability.JobProtocolFiles {
		t.Fatalf("created job job_protocol = %+v, want %d", created.JobProtocol, capability.JobProtocolFiles)
	}
	raw := mustRun(t, e.codexTestEnv, e.seedRawJob(t, u, "queued", nil, 0, 600))
	if raw.JobProtocol.Valid {
		t.Fatalf("a pre-change job row must have a NULL job_protocol, got %+v", raw.JobProtocol)
	}
}

// TestJobFilesGateClaimRunLiveDB: ClaimRun's job clause.
//
// CALIBRATION (ClaimRun claimant arm): remove `AND (r.job_protocol IS NULL OR 'job_files_v1' =
// ANY(@worker_protocol_caps::text[]))` from the first ClaimRun job clause; the first subtest then
// sees the job_runner_v1-only worker claim a new job.
func TestJobFilesGateClaimRunLiveDB(t *testing.T) {
	t.Run("new job: a job_runner_v1-only worker never claims it, kill-switch on or off, even after ClearRunRequiredCapabilities", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		jobID := e.newStampedJob(t, u)
		runnerOnly := e.seedWorkerRow(t, u, false, nil, jobCap)
		for _, aware := range []bool{false, true} {
			if _, err := e.q.ClaimRun(e.ctx, e.claimParams(u, runnerOnly, false, []string{jobCap}, aware)); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("capability_aware=%v: a job_runner_v1-only worker claimed a new-protocol job (err=%v)", aware, err)
			}
		}
		e.exec(`UPDATE runs SET status = 'awaiting_approval', required_capabilities = '{docker}' WHERE id = $1`, jobID)
		if n, err := e.q.ClearRunRequiredCapabilities(e.ctx, store.ClearRunRequiredCapabilitiesParams{ID: jobID, UserID: u}); err != nil || n != 1 {
			t.Fatalf("ClearRunRequiredCapabilities = %d, %v; want 1 row", n, err)
		}
		e.exec(`UPDATE runs SET status = 'queued' WHERE id = $1`, jobID)
		if _, err := e.q.ClaimRun(e.ctx, e.claimParams(u, runnerOnly, false, []string{jobCap}, false)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("job claimed by a job_runner_v1-only worker AFTER ClearRunRequiredCapabilities (err=%v)", err)
		}
		if s := e.status(t, jobID); s != "queued" {
			t.Fatalf("the new job must stay queued through the refused claims; status = %q", s)
		}
		withFiles := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
		run, err := e.q.ClaimRun(e.ctx, e.claimParams(u, withFiles, false, []string{jobCap, jobFilesCap}, false))
		if err != nil {
			t.Fatalf("a job_runner_v1+job_files_v1 worker must claim the new job: %v", err)
		}
		if run.ID != jobID || run.Kind != runkind.Job || run.Status != "claimed" {
			t.Fatalf("claimed = %v %s %s, want the new job in status claimed", run.ID, run.Kind, run.Status)
		}
	})

	t.Run("pre-change job (job_protocol NULL): a job_runner_v1-only worker still claims it", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		jobID := e.seedRawJob(t, u, "queued", nil, 0, 600)
		runnerOnly := e.seedWorkerRow(t, u, false, nil, jobCap)
		run, err := e.q.ClaimRun(e.ctx, e.claimParams(u, runnerOnly, false, []string{jobCap}, false))
		if err != nil {
			t.Fatalf("a job_runner_v1-only worker must still claim a pre-change job: %v", err)
		}
		if run.ID != jobID {
			t.Fatalf("claimed %v, want %v", run.ID, jobID)
		}
	})
}

// TestJobFilesGatePeerSpreadLiveDB: ClaimRun's PEER arm. A busy claimant defers a job only to an
// idle peer that could actually claim it, which for a new job needs job_files_v1 too.
//
// CALIBRATION (peer arm): remove `AND (r.job_protocol IS NULL OR 'job_files_v1' =
// ANY(p.protocol_capabilities))` from the peer job clause; the first subtest then defers a new job
// to a peer that can never claim it.
func TestJobFilesGatePeerSpreadLiveDB(t *testing.T) {
	cap2 := int32(2)
	claimantCaps := []string{jobCap, jobFilesCap}
	busyClaimant := func(e jobEnv, u uuid.UUID) uuid.UUID {
		me := e.seedWorkerRow(t, u, false, &cap2, claimantCaps...)
		e.seedRawJob(t, u, "running", &me, 30*time.Second, 600)
		return me
	}

	t.Run("new job: an idle job_runner_v1-only peer is not a spread target; the busy claimant claims", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		me := busyClaimant(e, u)
		e.seedWorkerRow(t, u, false, &cap2, jobCap)
		jobID := e.newStampedJob(t, u)
		run, err := e.q.ClaimRun(e.ctx, e.claimParams(u, me, false, claimantCaps, false))
		if err != nil {
			t.Fatalf("the busy claimant must claim: the runner-only peer cannot take a new job (err=%v)", err)
		}
		if run.ID != jobID {
			t.Fatalf("claimed %v, want %v", run.ID, jobID)
		}
	})
	t.Run("new job: an idle job_files_v1 peer is a spread target; the busy claimant defers", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		me := busyClaimant(e, u)
		e.seedWorkerRow(t, u, false, &cap2, claimantCaps...)
		jobID := e.newStampedJob(t, u)
		if _, err := e.q.ClaimRun(e.ctx, e.claimParams(u, me, false, claimantCaps, false)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("the busy claimant must defer to the capable idle peer (err=%v)", err)
		}
		if s := e.status(t, jobID); s != "queued" {
			t.Fatalf("deferred job must stay queued; status = %q", s)
		}
	})
	t.Run("pre-change job: an idle job_runner_v1-only peer is a spread target; the busy claimant defers", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		me := busyClaimant(e, u)
		e.seedWorkerRow(t, u, false, &cap2, jobCap)
		jobID := e.seedRawJob(t, u, "queued", nil, 0, 600)
		if _, err := e.q.ClaimRun(e.ctx, e.claimParams(u, me, false, claimantCaps, false)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("the busy claimant must defer a pre-change job to a runner-only peer that can still claim it (err=%v)", err)
		}
		if s := e.status(t, jobID); s != "queued" {
			t.Fatalf("deferred job must stay queued; status = %q", s)
		}
	})
}

// TestJobFilesGateUnservableEphemeralLiveDB: LockUnservableEphemeralJobWorkers (the D-A2 pass). An
// ephemeral worker that registered with job_runner_v1 alone cannot serve a new job, so the pass
// fails the job; the same worker serves a pre-change job and is left alone.
//
// CALIBRATION (jobs.sql arm): remove the job_protocol arm from LockUnservableEphemeralJobWorkers;
// the first subtest then finds nothing to fail.
func TestJobFilesGateUnservableEphemeralLiveDB(t *testing.T) {
	const deadline = 10 * time.Minute
	t.Run("new job, ephemeral worker registered with job_runner_v1 only: job failed", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		jobID := e.newStampedJob(t, u)
		w := e.seedEphemeralWorker(t, u, jobID)
		e.registerEphemeral(t, w, jobCap)
		n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, deadline)
		if err != nil || n != 1 {
			t.Fatalf("pass = %d, %v; want 1 failed job", n, err)
		}
		run := mustRun(t, e.codexTestEnv, jobID)
		if run.Status != "failed" || run.FailOrigin.String != "no_job_capable_worker" {
			t.Fatalf("job = %s / %q, want failed / no_job_capable_worker", run.Status, run.FailOrigin.String)
		}
	})
	t.Run("pre-change job, the same worker: untouched", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		jobID := e.seedRawJob(t, u, "queued", nil, 0, 600)
		w := e.seedEphemeralWorker(t, u, jobID)
		e.registerEphemeral(t, w, jobCap)
		if n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, deadline); err != nil || n != 0 {
			t.Fatalf("pass = %d, %v; want 0 (a job_runner_v1 worker still serves a pre-change job)", n, err)
		}
		if e.status(t, jobID) != "queued" || !e.workerExists(t, w.ID) {
			t.Fatal("the pre-change job and its ephemeral worker must be left alone")
		}
	})
}

// TestQueuedReasonJobFilesLiveDB: the queued-reason rung names the block only for a job that
// actually needs job_files_v1, and its text names the capability.
//
// CALIBRATION (count param): drop the requires_job_files predicate from
// CountOnlineWorkersSatisfyingJobRunner; the first subtest then sees the runner-only worker satisfy
// a new job and returns no reason.
func TestQueuedReasonJobFilesLiveDB(t *testing.T) {
	newRow := func(u uuid.UUID, stamped bool) store.ListActiveRunsForHealthRow {
		row := store.ListActiveRunsForHealthRow{ID: uuid.New(), UserID: u, Kind: runkind.Job, Status: "queued", Health: healthOK, Harness: harnessClaude}
		if stamped {
			row.JobProtocol = pgtype.Int2{Int16: capability.JobProtocolFiles, Valid: true}
		}
		return row
	}
	t.Run("new job, only a job_runner_v1 worker online: no capable worker", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		e.seedWorkerRow(t, u, false, nil, jobCap)
		if got := e.svc.queuedReason(e.ctx, time.Now(), newRow(u, true)); got != reasonNoJobCapableWorker {
			t.Fatalf("queuedReason = %q, want %q", got, reasonNoJobCapableWorker)
		}
		e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
		if got := e.svc.queuedReason(e.ctx, time.Now(), newRow(u, true)); got == reasonNoJobCapableWorker {
			t.Fatalf("queuedReason still %q with a job_files_v1 worker online", got)
		}
	})
	t.Run("pre-change job, only a job_runner_v1 worker online: not blocked", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		e.seedWorkerRow(t, u, false, nil, jobCap)
		if got := e.svc.queuedReason(e.ctx, time.Now(), newRow(u, false)); got == reasonNoJobCapableWorker {
			t.Fatalf("a pre-change job got %q although a job_runner_v1 worker can claim it", got)
		}
	})
	if !strings.Contains(reasonNoJobCapableWorker, jobCap) && strings.Contains(reasonNoJobCapableWorker, jobFilesCap) {
		t.Fatalf("reasonNoJobCapableWorker = %q must name both %s and %s", reasonNoJobCapableWorker, jobCap, jobFilesCap)
	}
}
