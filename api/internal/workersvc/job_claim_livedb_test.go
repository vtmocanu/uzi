package workersvc

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// job_claim_livedb_test.go covers PRD #1908 M3 (first half) against a live Postgres: the
// job_runner_v1 claim gate (ClaimRun, its peer mirror, CountOnlineWorkersClaimableForRun and the
// queued-reason rung), the job claim payload, the D-A2 unservable-ephemeral pass, the D-E park
// guards and the wall-clock backstop. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// database; run via ./e2e/run-store-it.sh.

const jobCap = capability.JobRunnerV1

// seedWorkerRow inserts an online worker with a fresh heartbeat and the given flags.
func (e jobEnv) seedWorkerRow(t *testing.T, userID uuid.UUID, docker bool, maxConcurrent *int32, protocolCaps ...string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if protocolCaps == nil {
		protocolCaps = []string{}
	}
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, protocol_capabilities, docker_enabled, max_concurrent_runs, last_heartbeat_at)
	        VALUES ($1, $2, $3, $4, 'online', $5, $6, $7, now())`,
		id, userID, "w-"+id.String()[:8], id[:], protocolCaps, docker, maxConcurrent)
	return id
}

// makeTokenDefault marks the user's Anthropic token as the default, so a claim's credential
// ladder resolves it (seedJobUser's token is created without the default flag).
func (e jobEnv) makeTokenDefault(t *testing.T, userID uuid.UUID) {
	t.Helper()
	e.exec(`UPDATE user_secrets SET is_default = true WHERE user_id = $1 AND kind = 'anthropic_token'`, userID)
}

// seedRawJob inserts a repo-less job run directly, at the given status/worker/start time.
func (e jobEnv) seedRawJob(t *testing.T, userID uuid.UUID, status string, workerID *uuid.UUID, startedAgo time.Duration, wall int32) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var started any
	if startedAgo > 0 {
		started = time.Now().Add(-startedAgo)
	}
	e.exec(`INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, auto_approve, required_capabilities,
	                          status, worker_id, budget_wall_seconds, started_at, claimed_at, claim_generation)
	        VALUES ($1, $2, 'job', 'research', 'job', 'prompt', true, '{}', $3, $4, $5, $6, $6, 1)`,
		id, userID, status, workerID, wall, started)
	return id
}

func (e jobEnv) claimParams(userID, workerID uuid.UUID, docker bool, protocolCaps []string, capAware bool) store.ClaimRunParams {
	now := time.Now()
	return store.ClaimRunParams{
		WorkerID:              pgtype.UUID{Bytes: workerID, Valid: true},
		UserID:                userID,
		HeartbeatCutoff:       pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
		AffinityCutoff:        pgtype.Timestamptz{Time: now.Add(-25 * time.Minute), Valid: true},
		SpreadCutoff:          pgtype.Timestamptz{Time: now.Add(-9 * time.Second), Valid: true},
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
		IsDockerWorker:        docker,
		WorkerCaps:            []string{},
		CapabilityAware:       capAware,
		WorkerProtocolCaps:    protocolCaps,
		CodexCuratedModels:    codexCuratedModelsSlice(),
	}
}

func (e jobEnv) status(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	return mustRun(t, e.codexTestEnv, runID).Status
}

func (e jobEnv) workerExists(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM workers WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// TestJobClaimHardClauseLiveDB: a queued job is claimable ONLY by a NON-docker worker advertising
// job_runner_v1, regardless of the capability_aware kill-switch and of ClearRunRequiredCapabilities.
//
// CALIBRATION (ClaimRun conjunct): remove the `AND (r.kind <> 'job' OR (NOT @is_docker_worker ...))`
// conjunct from ClaimRun and regenerate; step (1) then lets the incapable worker claim the job.
func TestJobClaimHardClauseLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	incapable := e.seedWorkerRow(t, u, false, nil)
	dockerCapable := e.seedWorkerRow(t, u, true, nil, jobCap)
	capable := e.seedWorkerRow(t, u, false, nil, jobCap)
	jobID := e.seedRawJob(t, u, "queued", nil, 0, 600)

	// (1) an old-image worker never claims a job, with capability_aware on or off.
	for _, aware := range []bool{false, true} {
		if _, err := e.q.ClaimRun(e.ctx, e.claimParams(u, incapable, false, []string{}, aware)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("capability_aware=%v: a worker without job_runner_v1 claimed a job (err=%v)", aware, err)
		}
	}
	// (2) a docker worker never claims a job, even advertising the capability.
	if _, err := e.q.ClaimRun(e.ctx, e.claimParams(u, dockerCapable, true, []string{jobCap}, false)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a docker worker claimed a job (err=%v)", err)
	}
	// (3) ClearRunRequiredCapabilities changes nothing: the clause is outside required_capabilities.
	e.exec(`UPDATE runs SET status = 'awaiting_approval', required_capabilities = '{docker}' WHERE id = $1`, jobID)
	cleared, err := e.q.ClearRunRequiredCapabilities(e.ctx, store.ClearRunRequiredCapabilitiesParams{ID: jobID, UserID: u})
	if err != nil || cleared != 1 {
		t.Fatalf("ClearRunRequiredCapabilities = %d, %v; want 1 row", cleared, err)
	}
	e.exec(`UPDATE runs SET status = 'queued' WHERE id = $1`, jobID)
	if _, err := e.q.ClaimRun(e.ctx, e.claimParams(u, incapable, false, []string{}, false)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("job claimed by an old-image worker AFTER ClearRunRequiredCapabilities (err=%v)", err)
	}
	if s := e.status(t, jobID); s != "queued" {
		t.Fatalf("job must stay queued through the refused claims; status = %q", s)
	}
	// (4) a non-docker job_runner_v1 worker claims it.
	run, err := e.q.ClaimRun(e.ctx, e.claimParams(u, capable, false, []string{jobCap}, false))
	if err != nil {
		t.Fatalf("a non-docker job_runner_v1 worker must claim the job: %v", err)
	}
	if run.ID != jobID || run.Status != "claimed" || run.Kind != runkind.Job {
		t.Fatalf("claimed = %v %s %s, want the job in status claimed", run.ID, run.Kind, run.Status)
	}
}

// TestJobClaimPeerSpreadMirrorLiveDB: fleet-aware spread must not defer a job to a peer that can
// never claim it. A busy capable claimant defers to a strictly-better idle peer only when that peer
// is itself job-capable.
//
// CALIBRATION (peer mirror): remove the `AND (r.kind <> 'job' OR (NOT COALESCE(p.docker_enabled, false)
// AND 'job_runner_v1' = ANY(p.protocol_capabilities)))` arm from the ClaimRun peer block and
// regenerate; the "old-image idle peer" subtest then defers to a peer that can never take the job.
func TestJobClaimPeerSpreadMirrorLiveDB(t *testing.T) {
	newBusyClaimant := func(e jobEnv, u uuid.UUID) uuid.UUID {
		cap2 := int32(2)
		me := e.seedWorkerRow(t, u, false, &cap2, jobCap)
		e.seedRawJob(t, u, "running", &me, 30*time.Second, 600) // one active run: not minimum-loaded
		return me
	}
	cap2 := int32(2)

	t.Run("old-image idle peer is not a spread target; busy claimant claims", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		me := newBusyClaimant(e, u)
		e.seedWorkerRow(t, u, false, &cap2) // idle, no job_runner_v1
		jobID := e.seedRawJob(t, u, "queued", nil, 0, 600)
		run, err := e.q.ClaimRun(e.ctx, e.claimParams(u, me, false, []string{jobCap}, false))
		if err != nil {
			t.Fatalf("busy claimant must claim: the old-image peer cannot take a job (err=%v)", err)
		}
		if run.ID != jobID {
			t.Fatalf("claimed %v, want %v", run.ID, jobID)
		}
	})
	t.Run("docker idle peer is not a spread target; busy claimant claims", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		me := newBusyClaimant(e, u)
		e.seedWorkerRow(t, u, true, &cap2, jobCap) // idle, advertises the cap, but docker
		jobID := e.seedRawJob(t, u, "queued", nil, 0, 600)
		run, err := e.q.ClaimRun(e.ctx, e.claimParams(u, me, false, []string{jobCap}, false))
		if err != nil {
			t.Fatalf("busy claimant must claim: a docker peer cannot take a job (err=%v)", err)
		}
		if run.ID != jobID {
			t.Fatalf("claimed %v, want %v", run.ID, jobID)
		}
	})
	t.Run("job-capable idle peer is a spread target; busy claimant defers", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		me := newBusyClaimant(e, u)
		e.seedWorkerRow(t, u, false, &cap2, jobCap)
		jobID := e.seedRawJob(t, u, "queued", nil, 0, 600)
		if _, err := e.q.ClaimRun(e.ctx, e.claimParams(u, me, false, []string{jobCap}, false)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("busy claimant must defer the job to the capable idle peer (err=%v)", err)
		}
		if s := e.status(t, jobID); s != "queued" {
			t.Fatalf("deferred job must stay queued; status = %q", s)
		}
	})
}

// TestCountOnlineWorkersClaimableForRunJobLiveDB: the per-run claimable count mirrors ClaimRun's
// job clause, so the health rung and the claim gate agree.
//
// CALIBRATION (count mirror): remove the job arm from CountOnlineWorkersClaimableForRun and
// regenerate; the old-image and docker workers then count as able to claim.
func TestCountOnlineWorkersClaimableForRunJobLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	jobID := e.seedRawJob(t, u, "queued", nil, 0, 600)
	count := func() int64 {
		n, err := e.q.CountOnlineWorkersClaimableForRun(e.ctx, store.CountOnlineWorkersClaimableForRunParams{
			RunID:              jobID,
			HeartbeatCutoff:    pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
			CapabilityAware:    true,
			CodexCuratedModels: codexCuratedModelsSlice(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	e.seedWorkerRow(t, u, false, nil)        // old image
	e.seedWorkerRow(t, u, true, nil, jobCap) // docker, advertising
	if n := count(); n != 0 {
		t.Fatalf("claimable workers = %d, want 0 (old image and docker cannot claim a job)", n)
	}
	e.seedWorkerRow(t, u, false, nil, jobCap)
	if n := count(); n != 1 {
		t.Fatalf("claimable workers = %d, want 1 (the non-docker job_runner_v1 worker)", n)
	}
}

// TestQueuedReasonNoJobCapableWorkerLiveDB drives the real queuedReason rung.
func TestQueuedReasonNoJobCapableWorkerLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	row := store.ListActiveRunsForHealthRow{ID: uuid.New(), UserID: u, Kind: runkind.Job, Status: "queued", Health: healthOK, Harness: harnessClaude}

	e.seedWorkerRow(t, u, false, nil) // old image only
	if got := e.svc.queuedReason(e.ctx, time.Now(), row); got != reasonNoJobCapableWorker {
		t.Fatalf("queuedReason = %q, want %q", got, reasonNoJobCapableWorker)
	}
	e.seedWorkerRow(t, u, false, nil, jobCap)
	if got := e.svc.queuedReason(e.ctx, time.Now(), row); got == reasonNoJobCapableWorker {
		t.Fatalf("queuedReason still %q with a job-capable worker online", got)
	}
	issueRow := row
	issueRow.Kind = runkind.Issue
	e2 := setupJobLiveDB(t, 0)
	u2 := e2.seedJobUser(t)
	issueRow.UserID = u2
	e2.seedWorkerRow(t, u2, false, nil)
	if got := e2.svc.queuedReason(e2.ctx, time.Now(), issueRow); got == reasonNoJobCapableWorker {
		t.Fatalf("an issue run must never get the job rung; got %q", got)
	}
}

// TestJobClaimPayloadLiveDB: a real svc.Claim of a job by a job-capable worker returns the job
// block and the model credential, and NOTHING repo-shaped: no PAT, no repo, no custody hold even
// for a recovery-capable worker. An old-image worker and a docker worker get nothing.
func TestJobClaimPayloadLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	e.makeTokenDefault(t, u)
	p := jobReq(cliCaller(u))
	p.Inputs = []JobInput{{Name: "notes.md", Content: "alpha"}, {Name: "data.csv", Content: "a,b"}}
	wall := 900
	p.WallSeconds = &wall
	v, err := e.svc.CreateJobRun(e.ctx, p)
	if err != nil {
		t.Fatalf("CreateJobRun: %v", err)
	}

	oldID := e.seedWorkerRow(t, u, false, nil)
	old := store.Worker{ID: oldID, UserID: u, Name: "old", Status: "online"}
	if got, err := e.svc.Claim(e.ctx, old, nil); err != nil || got != nil {
		t.Fatalf("old-image worker Claim = %v, %v; want idle", got, err)
	}
	dockID := e.seedWorkerRow(t, u, true, nil, jobCap)
	dock := store.Worker{ID: dockID, UserID: u, Name: "dock", Status: "online", DockerEnabled: pgtype.Bool{Bool: true, Valid: true}, ProtocolCapabilities: []string{jobCap}}
	if got, err := e.svc.Claim(e.ctx, dock, nil); err != nil || got != nil {
		t.Fatalf("docker worker Claim = %v, %v; want idle", got, err)
	}

	capID := e.seedWorkerRow(t, u, false, nil, jobCap, capability.RecoveryArchiveV1)
	capable := store.Worker{ID: capID, UserID: u, Name: "capable", Status: "online", ProtocolCapabilities: []string{capability.RecoveryArchiveV1, jobCap}}
	pl, err := e.svc.Claim(e.ctx, capable, nil)
	if err != nil {
		t.Fatalf("Claim(capable): %v", err)
	}
	if pl == nil || pl.RunID != v.ID.String() {
		t.Fatalf("Claim = %+v, want the job %s", pl, v.ID)
	}
	if pl.Kind != runkind.Job || pl.Job == nil || pl.Job.Type != "research" || pl.Job.Title != "Summarize" || pl.Job.Prompt != p.Prompt {
		t.Fatalf("job block = %+v (kind %q)", pl.Job, pl.Kind)
	}
	if len(pl.Job.Inputs) != 2 || pl.Job.Inputs[0] != (ClaimJobInput{Name: "notes.md", Content: "alpha"}) || pl.Job.Inputs[1] != (ClaimJobInput{Name: "data.csv", Content: "a,b"}) {
		t.Fatalf("job inputs = %+v", pl.Job.Inputs)
	}
	if pl.BudgetWallSeconds == nil || *pl.BudgetWallSeconds != 900 || pl.Config.RunTimeoutSeconds != 900 {
		t.Fatalf("wall budget = %v / %d, want 900", pl.BudgetWallSeconds, pl.Config.RunTimeoutSeconds)
	}
	if pl.Secrets.AnthropicOAuthToken == "" {
		t.Fatal("the job claim must carry the model credential")
	}
	if pl.Secrets.ForgePAT != "" || pl.Secrets.ForgeUsername != "" || pl.Secrets.Codex != nil {
		t.Fatalf("a job claim must carry no forge or codex secret: %+v", pl.Secrets)
	}
	if pl.Repo != (ClaimRepo{}) || pl.Branch != nil || pl.IssueIID != nil || len(pl.Agents) != 0 || len(pl.Skills) != 0 {
		t.Fatalf("a job claim must carry no repo, branch, issue, agents or skills: %+v", pl)
	}
	if pl.ClaimGeneration != 1 {
		t.Fatalf("claim generation = %d, want 1", pl.ClaimGeneration)
	}
	var holds int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1`, v.ID).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if holds != 0 {
		t.Fatalf("a job claim opened %d custody holds, want 0", holds)
	}
	raw, err := json.Marshal(pl)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["job"]; !ok {
		t.Fatal("the wire claim must carry the job key")
	}
	if secrets, _ := m["secrets"].(map[string]any); secrets["forge_pat"] != "" {
		t.Fatalf("wire forge_pat = %v, want empty", secrets["forge_pat"])
	}
}

// TestJobEmptyAutoPoolFailsClosedLiveDB: a job claimed by an auto-bound worker whose owner pooled no
// token is FAILED closed (credential_unavailable), never requeued into pool_wait. A job never parks
// (D-E) and RequeueClaimAssemblyExact excludes it from pool_wait, so a requeue would match no row,
// roll the claim back and strand the run in 'claimed' for the never-started sweep to re-claim, in a
// loop the wall backstop never breaks (claimed_at resets).
//
// MUTATION CHECK: removing jobCredentialErr's errAutoPoolEmpty arm makes Claim return an error and
// leaves the run 'claimed', and this test goes red.
func TestJobEmptyAutoPoolFailsClosedLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t) // one enabled, non-default token that is not pooled: the auto pool is empty
	v, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
	if err != nil {
		t.Fatal(err)
	}
	workerID := e.seedWorkerRow(t, u, false, nil, jobCap)
	e.exec(`UPDATE workers SET anthropic_bind_mode = 'auto' WHERE id = $1`, workerID)
	wkr := store.Worker{ID: workerID, UserID: u, Name: "auto", Status: "online", ProtocolCapabilities: []string{jobCap}, AnthropicBindMode: BindModeAuto}

	pl, err := e.svc.Claim(e.ctx, wkr, nil)
	if err != nil || pl != nil {
		t.Fatalf("Claim = %+v, %v; want idle with no error (the claim must settle, not loop)", pl, err)
	}
	run := mustRun(t, e.codexTestEnv, v.ID)
	if run.Status != "failed" || run.FailOrigin.String != "credential_unavailable" || run.FailureReason.String == "" || !run.FinishedAt.Valid {
		t.Fatalf("job = %s / %q / reason %q / finished %v; want failed / credential_unavailable with a reason", run.Status, run.FailOrigin.String, run.FailureReason.String, run.FinishedAt.Valid)
	}
	if run.RequeueCount != 0 {
		t.Fatalf("requeue_count = %d, want 0", run.RequeueCount)
	}
	// A second claim finds nothing: the job is terminal, not re-queued.
	if pl, err := e.svc.Claim(e.ctx, wkr, nil); err != nil || pl != nil {
		t.Fatalf("second Claim = %+v, %v; want idle", pl, err)
	}
	if got := e.status(t, v.ID); got != "failed" {
		t.Fatalf("status after the second claim = %q, want failed", got)
	}
}

// TestJobEndToEndEphemeralProvisionRegisterClaimLiveDB: a job with only an old-image worker online
// is a gap-trigger candidate; a run-bound ephemeral worker inserted exactly as the provisioner does
// it, registered with job_runner_v1 through the real register path, claims it.
func TestJobEndToEndEphemeralProvisionRegisterClaimLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	e.makeTokenDefault(t, u)
	e.exec(`UPDATE users SET ephemeral_workers_enabled = true WHERE id = $1`, u)
	oldID := e.seedWorkerRow(t, u, false, nil)
	v, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
	if err != nil {
		t.Fatal(err)
	}
	inGap := func() bool {
		rows, err := e.q.ListUnplaceableQueuedRunsForEphemeral(e.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{MaxRows: 1000, MaxPerUser: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == v.ID {
				return true
			}
		}
		return false
	}
	if !inGap() {
		t.Fatal("the job must be a gap-trigger candidate while only an old-image worker is online")
	}
	if got, err := e.svc.Claim(e.ctx, store.Worker{ID: oldID, UserID: u, Name: "old", Status: "online"}, nil); err != nil || got != nil {
		t.Fatalf("old-image Claim = %v, %v; want idle", got, err)
	}

	eph := e.seedEphemeralWorker(t, u, v.ID)
	if inGap() {
		t.Fatal("a job with a bound ephemeral worker must not be re-surfaced (one per run)")
	}
	registered, _, err := e.svc.Register(e.ctx, eph, "test", "", nil, nil, []string{jobCap}, nil)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !registered.OnlineSince.Valid {
		t.Fatal("register must mark the ephemeral worker online")
	}
	pl, err := e.svc.Claim(e.ctx, registered, nil)
	if err != nil {
		t.Fatalf("Claim(ephemeral): %v", err)
	}
	if pl == nil || pl.RunID != v.ID.String() || pl.Job == nil {
		t.Fatalf("the bound ephemeral worker must claim its job; got %+v", pl)
	}
	run := mustRun(t, e.codexTestEnv, v.ID)
	if run.Status != "claimed" || !run.WorkerID.Valid || uuid.UUID(run.WorkerID.Bytes) != registered.ID {
		t.Fatalf("run = %s worker %+v, want claimed by the ephemeral worker", run.Status, run.WorkerID)
	}
}

// seedEphemeralWorker inserts a run-bound ephemeral hosted worker exactly as the provisioner does
// (CreateEphemeralHostedWorker, docker off), never registered.
func (e jobEnv) seedEphemeralWorker(t *testing.T, userID, runID uuid.UUID) store.Worker {
	t.Helper()
	hash := make([]byte, 32)
	if _, err := rand.Read(hash); err != nil {
		t.Fatal(err)
	}
	w, err := e.q.CreateEphemeralHostedWorker(e.ctx, store.CreateEphemeralHostedWorkerParams{
		UserID:            userID,
		Name:              "ephemeral-" + runID.String(),
		TokenHash:         hash,
		TemplateDeclared:  pgtype.Text{String: "base", Valid: true},
		HostedSize:        pgtype.Text{String: "small", Valid: true},
		DockerEnabled:     pgtype.Bool{Bool: false, Valid: true},
		EphemeralRunID:    runID,
		AnthropicBindMode: BindModeDefault,
	})
	if err != nil {
		t.Fatalf("CreateEphemeralHostedWorker: %v", err)
	}
	return store.Worker(w)
}

// registerEphemeral brings a bound ephemeral worker online with the given protocol capabilities.
func (e jobEnv) registerEphemeral(t *testing.T, w store.Worker, protocolCaps ...string) {
	t.Helper()
	if _, _, err := e.svc.Register(e.ctx, w, "test", "", nil, nil, protocolCaps, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

func (e jobEnv) ageWorker(t *testing.T, id uuid.UUID, by time.Duration) {
	t.Helper()
	e.exec(`UPDATE workers SET created_at = now() - $2::interval WHERE id = $1`, id, pgtype.Interval{Microseconds: by.Microseconds(), Valid: true})
}

// TestFailJobsWithUnservableEphemeralLiveDB covers PRD #1908 D-A2.
//
// CALIBRATION (race guard): remove `AND status = 'queued' AND worker_id IS NULL` from
// FailUnservedJobRun and regenerate; the "claimed elsewhere" subtest then fails, because the pass
// flips a job another worker already claimed to failed.
func TestFailJobsWithUnservableEphemeralLiveDB(t *testing.T) {
	const deadline = 10 * time.Minute

	t.Run("online without job_runner_v1: job failed, worker gone", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		v, _ := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
		w := e.seedEphemeralWorker(t, u, v.ID)
		e.registerEphemeral(t, w) // old image: no job_runner_v1

		n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, deadline)
		if err != nil || n != 1 {
			t.Fatalf("pass = %d, %v; want 1 failed job", n, err)
		}
		run := mustRun(t, e.codexTestEnv, v.ID)
		if run.Status != "failed" || run.FailOrigin.String != "no_job_capable_worker" || !run.FinishedAt.Valid || run.FailureReason.String == "" {
			t.Fatalf("job = %s / %q / finished %v / reason %q", run.Status, run.FailOrigin.String, run.FinishedAt.Valid, run.FailureReason.String)
		}
		if e.workerExists(t, w.ID) {
			t.Fatal("the unservable ephemeral worker row must be deleted")
		}
	})

	t.Run("never registered past the deadline: job failed, slot freed, no re-provision", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		e.exec(`UPDATE users SET ephemeral_workers_enabled = true WHERE id = $1`, u)
		v, _ := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
		w := e.seedEphemeralWorker(t, u, v.ID)
		e.ageWorker(t, w.ID, 20*time.Minute)

		n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, deadline)
		if err != nil || n != 1 {
			t.Fatalf("pass = %d, %v; want 1 failed job", n, err)
		}
		run := mustRun(t, e.codexTestEnv, v.ID)
		if run.Status != "failed" || run.FailOrigin.String != "ephemeral_worker_never_registered" {
			t.Fatalf("job = %s / %q", run.Status, run.FailOrigin.String)
		}
		if e.workerExists(t, w.ID) {
			t.Fatal("the never-registered worker row must be deleted")
		}
		if c, err := e.q.CountEphemeralHostedWorkersForUser(e.ctx, u); err != nil || c != 0 {
			t.Fatalf("ephemeral slot count = %d, %v; want 0 (slot freed)", c, err)
		}
		rows, err := e.q.ListUnplaceableQueuedRunsForEphemeral(e.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{MaxRows: 1000, MaxPerUser: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == v.ID {
				t.Fatal("the gap trigger must not re-provision for a failed job")
			}
		}
	})

	t.Run("never registered but inside the deadline: untouched", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		v, _ := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
		w := e.seedEphemeralWorker(t, u, v.ID)
		e.ageWorker(t, w.ID, time.Minute)
		if n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, deadline); err != nil || n != 0 {
			t.Fatalf("pass = %d, %v; want 0", n, err)
		}
		if e.status(t, v.ID) != "queued" || !e.workerExists(t, w.ID) {
			t.Fatal("a worker still booting must keep its job queued and its row")
		}
	})

	t.Run("capable worker registered: untouched", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		v, _ := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
		w := e.seedEphemeralWorker(t, u, v.ID)
		e.registerEphemeral(t, w, jobCap)
		if n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, deadline); err != nil || n != 0 {
			t.Fatalf("pass = %d, %v; want 0", n, err)
		}
		if e.status(t, v.ID) != "queued" || !e.workerExists(t, w.ID) {
			t.Fatal("a job-capable ephemeral worker must keep its job and its row")
		}
	})

	t.Run("job claimed elsewhere first: job untouched, stale ephemeral row deleted", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		v, _ := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
		w := e.seedEphemeralWorker(t, u, v.ID)
		e.registerEphemeral(t, w) // old image, online: unservable
		capable := e.seedWorkerRow(t, u, false, nil, jobCap)
		run, err := e.q.ClaimRun(e.ctx, e.claimParams(u, capable, false, []string{jobCap}, false))
		if err != nil || run.ID != v.ID {
			t.Fatalf("capable worker ClaimRun = %v, %v; want the job", run.ID, err)
		}

		n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, deadline)
		if err != nil || n != 0 {
			t.Fatalf("pass = %d, %v; want 0 failed jobs", n, err)
		}
		after := mustRun(t, e.codexTestEnv, v.ID)
		if after.Status != "claimed" || !after.WorkerID.Valid || uuid.UUID(after.WorkerID.Bytes) != capable || after.FailOrigin.Valid {
			t.Fatalf("job = %s worker %+v fail_origin %+v; want it left claimed by the capable worker", after.Status, after.WorkerID, after.FailOrigin)
		}
		if e.workerExists(t, w.ID) {
			t.Fatal("the stale ephemeral row must still be deleted")
		}
	})

	// goStale reproduces what MarkStaleWorkersOffline does to a registered worker whose heartbeats
	// stopped: offline, online_since cleared. last_heartbeat_at stays (it is the registration
	// signal), and the row is old enough to be past the provision deadline.
	goStale := func(e jobEnv, t *testing.T, id uuid.UUID) {
		t.Helper()
		e.exec(`UPDATE workers SET status = 'offline', online_since = NULL, last_heartbeat_at = now() - interval '1 hour' WHERE id = $1`, id)
		e.ageWorker(t, id, 20*time.Minute)
	}

	t.Run("registered capable worker gone heartbeat-stale: NOT failed as never-registered", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		v, _ := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
		w := e.seedEphemeralWorker(t, u, v.ID)
		e.registerEphemeral(t, w, jobCap)
		goStale(e, t, w.ID)
		if n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, deadline); err != nil || n != 0 {
			t.Fatalf("pass = %d, %v; want 0 (the worker registered, it merely went stale)", n, err)
		}
		if e.status(t, v.ID) != "queued" || !e.workerExists(t, w.ID) {
			t.Fatal("a stale but job-capable ephemeral worker must leave its job queued and its row for the reaper")
		}
	})

	t.Run("registered incapable worker gone offline: failed as no_job_capable_worker", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		v, _ := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
		w := e.seedEphemeralWorker(t, u, v.ID)
		e.registerEphemeral(t, w) // old image
		goStale(e, t, w.ID)
		if n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, deadline); err != nil || n != 1 {
			t.Fatalf("pass = %d, %v; want 1", n, err)
		}
		run := mustRun(t, e.codexTestEnv, v.ID)
		if run.Status != "failed" || run.FailOrigin.String != "no_job_capable_worker" {
			t.Fatalf("job = %s / %q; want failed / no_job_capable_worker, not never-registered", run.Status, run.FailOrigin.String)
		}
		if e.workerExists(t, w.ID) {
			t.Fatal("the unservable worker row must be deleted")
		}
	})

	t.Run("docker ephemeral advertising job_runner_v1 is unservable", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		v, _ := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
		w := e.seedEphemeralWorker(t, u, v.ID)
		e.registerEphemeral(t, w, jobCap)
		e.exec(`UPDATE workers SET docker_enabled = true WHERE id = $1`, w.ID)
		if n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, deadline); err != nil || n != 1 {
			t.Fatalf("pass = %d, %v; want 1", n, err)
		}
		if run := mustRun(t, e.codexTestEnv, v.ID); run.FailOrigin.String != "no_job_capable_worker" {
			t.Fatalf("fail_origin = %q; a docker worker can never claim a job", run.FailOrigin.String)
		}
	})

	// CALIBRATION (each FailUnservedJobRun race-guard conjunct alone): drop `AND worker_id IS NULL`
	// and the "queued but already bound" subtest fails; drop `AND status = 'queued'` and the
	// "unbound but not queued" subtest fails.
	t.Run("queued job already bound to a worker (requeue affinity): untouched", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		v, _ := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
		w := e.seedEphemeralWorker(t, u, v.ID)
		e.registerEphemeral(t, w) // incapable
		other := e.seedWorkerRow(t, u, false, nil, jobCap)
		e.exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, v.ID, other)
		if n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, deadline); err != nil || n != 0 {
			t.Fatalf("pass = %d, %v; want 0 failed jobs", n, err)
		}
		after := mustRun(t, e.codexTestEnv, v.ID)
		if after.Status != "queued" || after.FailOrigin.Valid {
			t.Fatalf("job = %s / %+v; a job bound to a worker must not be failed by the D-A2 pass", after.Status, after.FailOrigin)
		}
		if e.workerExists(t, w.ID) {
			t.Fatal("the stale ephemeral row must still be deleted")
		}
	})

	t.Run("unbound job that is not queued (cancelled): untouched", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		v, _ := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
		w := e.seedEphemeralWorker(t, u, v.ID)
		e.registerEphemeral(t, w) // incapable
		e.exec(`UPDATE runs SET status = 'cancelled', finished_at = now() WHERE id = $1`, v.ID)
		if n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, deadline); err != nil || n != 0 {
			t.Fatalf("pass = %d, %v; want 0 failed jobs", n, err)
		}
		after := mustRun(t, e.codexTestEnv, v.ID)
		if after.Status != "cancelled" || after.FailOrigin.Valid {
			t.Fatalf("job = %s / %+v; a cancelled job must stay cancelled", after.Status, after.FailOrigin)
		}
	})

	t.Run("an ephemeral worker bound to a non-job run is never touched", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		env := setupInterlockLiveDB(t)
		runID := env.seedQueuedRun(t, nil, nil)
		w := e.seedEphemeralWorker(t, env.userID, runID)
		e.registerEphemeral(t, w)
		e.ageWorker(t, w.ID, 20*time.Minute)
		if n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, deadline); err != nil || n != 0 {
			t.Fatalf("pass = %d, %v; want 0", n, err)
		}
		if !e.workerExists(t, w.ID) || env.runStatus(t, runID) != "queued" {
			t.Fatal("an issue run's ephemeral worker and run are none of this pass's business")
		}
	})
}

// TestJobParkGuardsLiveDB: a job never parks (PRD #1908 D-E). Each park writer is a no-op for a
// job and works for an issue run on the same worker, which proves the fixture is live.
func TestJobParkGuardsLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	env := setupInterlockLiveDB(t)
	u := env.userID
	w := e.seedWorkerRow(t, u, false, nil, jobCap)
	wid := pgtype.UUID{Bytes: w, Valid: true}
	job := e.seedRawJob(t, u, "running", &w, time.Minute, 600)
	issue := env.seedActiveRunOwnedBy(t, w)

	t.Run("SetRunLimitWait", func(t *testing.T) {
		mk := func(id uuid.UUID) store.SetRunLimitWaitParams {
			return store.SetRunLimitWaitParams{ID: id, WorkerID: wid, LimitResetsAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}, RateLimitType: pgtype.Text{String: "five_hour", Valid: true}}
		}
		if n, err := e.q.SetRunLimitWait(e.ctx, mk(job)); err != nil || n != 0 {
			t.Fatalf("job = %d, %v; want a no-op", n, err)
		}
		if n, err := e.q.SetRunLimitWait(e.ctx, mk(issue)); err != nil || n != 1 {
			t.Fatalf("control issue run = %d, %v; want 1 row", n, err)
		}
	})
	t.Run("SetRunRecoveryWait", func(t *testing.T) {
		env.exec(t, `UPDATE runs SET status = 'running' WHERE id = $1`, issue)
		mk := func(id uuid.UUID) store.SetRunRecoveryWaitParams {
			return store.SetRunRecoveryWaitParams{ID: id, WorkerID: wid, RecoveryCause: pgtype.Text{String: "provider_outage", Valid: true}, RetryNotBefore: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}}
		}
		if n, err := e.q.SetRunRecoveryWait(e.ctx, mk(job)); err != nil || n != 0 {
			t.Fatalf("job = %d, %v; want a no-op", n, err)
		}
		if n, err := e.q.SetRunRecoveryWait(e.ctx, mk(issue)); err != nil || n != 1 {
			t.Fatalf("control issue run = %d, %v; want 1 row", n, err)
		}
	})
	t.Run("ParkRunForgeUnreachable", func(t *testing.T) {
		env.exec(t, `UPDATE runs SET status = 'running' WHERE id = $1`, issue)
		mk := func(id uuid.UUID) store.ParkRunForgeUnreachableParams {
			return store.ParkRunForgeUnreachableParams{ID: id, WorkerID: wid, RetryNotBefore: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}}
		}
		if _, err := e.q.ParkRunForgeUnreachable(e.ctx, mk(job)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("job err = %v; want no rows", err)
		}
		if _, err := e.q.ParkRunForgeUnreachable(e.ctx, mk(issue)); err != nil {
			t.Fatalf("control issue run: %v", err)
		}
	})
	t.Run("ParkRunDataVolumeFull", func(t *testing.T) {
		env.exec(t, `UPDATE runs SET status = 'running', claim_released_at = NULL WHERE id = $1`, issue)
		mk := func(id uuid.UUID) store.ParkRunDataVolumeFullParams {
			return store.ParkRunDataVolumeFullParams{ID: id, WorkerID: wid, RetryNotBefore: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}}
		}
		if _, err := e.q.ParkRunDataVolumeFull(e.ctx, mk(job)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("job err = %v; want no rows", err)
		}
		if _, err := e.q.ParkRunDataVolumeFull(e.ctx, mk(issue)); err != nil {
			t.Fatalf("control issue run: %v", err)
		}
	})
	t.Run("RequeueClaimAssemblyExact pool_wait", func(t *testing.T) {
		jc := e.seedRawJob(t, u, "claimed", &w, 0, 600)
		ic := env.seedActiveRunOwnedBy(t, w)
		env.exec(t, `UPDATE runs SET status = 'claimed', claim_generation = 1 WHERE id = $1`, ic)
		mk := func(id uuid.UUID, pool bool) store.RequeueClaimAssemblyExactParams {
			return store.RequeueClaimAssemblyExactParams{ID: id, WorkerID: wid, ClaimGeneration: 1, PoolWait: pool}
		}
		if n, err := e.q.RequeueClaimAssemblyExact(e.ctx, mk(jc, true)); err != nil || n != 0 {
			t.Fatalf("job pool_wait = %d, %v; want a no-op", n, err)
		}
		if n, err := e.q.RequeueClaimAssemblyExact(e.ctx, mk(ic, true)); err != nil || n != 1 {
			t.Fatalf("control issue pool_wait = %d, %v; want 1 row", n, err)
		}
		if n, err := e.q.RequeueClaimAssemblyExact(e.ctx, mk(jc, false)); err != nil || n != 1 {
			t.Fatalf("job plain requeue = %d, %v; want 1 row (only the hold is barred)", n, err)
		}
	})
	t.Run("CreateExtendInput", func(t *testing.T) {
		mk := func(id uuid.UUID) store.CreateExtendInputParams {
			return store.CreateExtendInputParams{ID: id, Body: pgtype.Text{String: "extend", Valid: true}, Secs: 600, Cap: 28800}
		}
		if _, err := e.q.CreateExtendInput(e.ctx, mk(job)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("job err = %v; want no rows", err)
		}
		if _, err := e.q.CreateExtendInput(e.ctx, mk(issue)); err != nil {
			t.Fatalf("control issue run: %v", err)
		}
	})
}

// TestJobWallPassesSkipJobLiveDB: the wall-park passes never touch a job, while an issue run in
// the same position is requested / parked (the control).
func TestJobWallPassesSkipJobLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	env := setupInterlockLiveDB(t)
	u := env.userID
	now := time.Now()
	past := 10 * time.Hour
	params := store.RequestWallParksParams{Now: pgtype.Timestamptz{Time: now, Valid: true}, GlobalTimeoutSeconds: 7200, WorkerStaleCutoff: pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true}}

	// RequestWallParks: a live, wall_park_v1 worker holding an out-of-time run.
	wj := e.seedWorkerRow(t, u, false, nil, jobCap, capability.WallParkV1)
	wi := e.seedWorkerRow(t, u, false, nil, capability.WallParkV1)
	job := e.seedRawJob(t, u, "running", &wj, past, 600)
	issue := env.seedActiveRunOwnedBy(t, wi)
	env.exec(t, `UPDATE runs SET started_at = now() - interval '10 hours', budget_wall_seconds = 600 WHERE id = $1`, issue)
	rows, err := e.q.RequestWallParks(e.ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	var gotJob, gotIssue bool
	for _, r := range rows {
		gotJob = gotJob || r.ID == job
		gotIssue = gotIssue || r.ID == issue
	}
	if gotJob || !gotIssue {
		t.Fatalf("RequestWallParks job=%v issue=%v; want job skipped, issue requested", gotJob, gotIssue)
	}

	// ParkRunsAtWall: a worker WITHOUT wall_park_v1 (incapable) holding an out-of-time run.
	wj2 := e.seedWorkerRow(t, u, false, nil, jobCap)
	wi2 := e.seedWorkerRow(t, u, false, nil)
	job2 := e.seedRawJob(t, u, "running", &wj2, past, 600)
	issue2 := env.seedActiveRunOwnedBy(t, wi2)
	env.exec(t, `UPDATE runs SET started_at = now() - interval '10 hours', budget_wall_seconds = 600 WHERE id = $1`, issue2)
	parked, err := e.q.ParkRunsAtWall(e.ctx, store.ParkRunsAtWallParams{Now: params.Now, GlobalTimeoutSeconds: 7200, WorkerStaleCutoff: params.WorkerStaleCutoff, GraceSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	gotJob, gotIssue = false, false
	for _, r := range parked {
		gotJob = gotJob || r.ID == job2
		gotIssue = gotIssue || r.ID == issue2
	}
	if gotJob || !gotIssue {
		t.Fatalf("ParkRunsAtWall job=%v issue=%v; want job skipped, issue parked", gotJob, gotIssue)
	}
	if s := e.status(t, job2); s != "running" {
		t.Fatalf("job status after the wall passes = %q, want running", s)
	}
}

// TestFailJobsPastWallDeadlineLiveDB: the D-E backstop fails a claimed or running job past its
// budget plus the grace, leaves one within budget, and never touches a non-job run.
func TestFailJobsPastWallDeadlineLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	env := setupInterlockLiveDB(t)
	u := env.userID
	w := e.seedWorkerRow(t, u, false, nil, jobCap)
	over := e.seedRawJob(t, u, "running", &w, 600*time.Second+jobWallBackstopGraceSeconds*time.Second+30*time.Second, 600)
	inGrace := e.seedRawJob(t, u, "running", &w, 700*time.Second, 600)
	fresh := e.seedRawJob(t, u, "running", &w, 30*time.Second, 600)
	claimedOver := e.seedRawJob(t, u, "claimed", &w, 3*time.Hour, 600)
	issue := env.seedActiveRunOwnedBy(t, w)
	env.exec(t, `UPDATE runs SET started_at = now() - interval '10 hours', budget_wall_seconds = 600 WHERE id = $1`, issue)

	if _, err := e.svc.FailJobsPastWallDeadline(e.ctx, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{over, claimedOver} {
		r := mustRun(t, e.codexTestEnv, id)
		if r.Status != "failed" || r.FailOrigin.String != "run_timeout" || !r.FinishedAt.Valid || r.FailureReason.String == "" {
			t.Fatalf("past-deadline job %s = %s / %q", id, r.Status, r.FailOrigin.String)
		}
	}
	for name, id := range map[string]uuid.UUID{"inside the grace": inGrace, "inside the budget": fresh} {
		if s := e.status(t, id); s != "running" {
			t.Fatalf("job %s status = %q, want running", name, s)
		}
	}
	if s := env.runStatus(t, issue); s != "running" {
		t.Fatalf("a non-job run must never be failed by the job backstop; status = %q", s)
	}
}
