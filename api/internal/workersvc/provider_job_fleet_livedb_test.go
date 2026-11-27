package workersvc

import (
	"context"
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

// provider_job_fleet_livedb_test.go is PRD #4322 M1b's SQL census (D14) against a real Postgres: a
// provider-bound job is claimable and placeable ONLY where a non-Docker worker advertises job_runner_v1,
// job_files_v1, claude_provider_v1 and claude_provider_job_v1, at every claim, peer, count, demand,
// wake, health and unservable-ephemeral site, with vendor jobs untouched. The clause texts are pinned
// without a database in store.TestProviderJobCapabilityClauseFormsPinned; these run the statements.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

var (
	pjobFullWorkerCaps = []string{jobCap, jobFilesCap, capability.ClaudeProviderV1, capability.ClaudeProviderJobV1}
	// An old job worker: it can run vendor jobs, and knows no provider capability.
	pjobOldWorkerCaps = []string{jobCap, jobFilesCap}
	// claude_provider_v1 without the job capability: it runs provider runs, not provider jobs.
	pjobNoJobCapCaps = []string{jobCap, jobFilesCap, capability.ClaudeProviderV1}
	// the job capability without claude_provider_v1: never advertised alone, but the clause needs both.
	pjobNoProviderCaps = []string{jobCap, jobFilesCap, capability.ClaudeProviderJobV1}
)

func (p *pjobFix) claimable(runID uuid.UUID) int64 {
	p.t.Helper()
	n, err := p.env.q.CountOnlineWorkersClaimableForRun(p.env.ctx, store.CountOnlineWorkersClaimableForRunParams{
		RunID:              runID,
		HeartbeatCutoff:    pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		CapabilityAware:    true,
		CodexCuratedModels: codexCuratedModelsSlice(),
	})
	if err != nil {
		p.t.Fatal(err)
	}
	return n.Claimable
}

func (p *pjobFix) inEphemeralGap(runID uuid.UUID) bool {
	p.t.Helper()
	p.env.exec(`UPDATE users SET ephemeral_workers_enabled = true WHERE id = $1`, p.userID)
	rows, err := p.env.q.ListUnplaceableQueuedRunsForEphemeral(p.env.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-15 * time.Minute), Valid: true}, MaxRows: 1000, MaxPerUser: 1000,
	})
	if err != nil {
		p.t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == runID {
			return true
		}
	}
	return false
}

func (p *pjobFix) tryClaim(worker uuid.UUID, docker bool, caps []string) (store.Run, error) {
	return p.env.q.ClaimRun(p.env.ctx, p.je.claimParams(p.userID, worker, docker, caps, false))
}

// TestProviderJobFleetClaimLiveDB: ClaimRun (both arms), the claimable count and the peer mirror.
func TestProviderJobFleetClaimLiveDB(t *testing.T) {
	t.Run("negative control: a fully capable Docker worker and an old non-Docker job worker", func(t *testing.T) {
		p := newPJobFix(t)
		r := p.mustCreate()
		old := p.je.seedWorkerRow(t, p.userID, false, nil, pjobOldWorkerCaps...)
		docker := p.je.seedWorkerRow(t, p.userID, true, nil, pjobFullWorkerCaps...)
		if _, err := p.tryClaim(old, false, pjobOldWorkerCaps); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("an old non-Docker job worker claimed a provider job (err=%v)", err)
		}
		if _, err := p.tryClaim(docker, true, pjobFullWorkerCaps); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("a Docker worker claimed a provider job (err=%v)", err)
		}
		if n := p.claimable(r.ID); n != 0 {
			t.Fatalf("claimable workers = %d, want 0", n)
		}
		if !p.inEphemeralGap(r.ID) {
			t.Fatal("the job must be ephemeral demand: the job arm sees no worker that can serve it")
		}
		if got := mustRun(t, p.env, r.ID).Status; got != "queued" {
			t.Fatalf("status = %s, want queued through the refused claims", got)
		}
	})
	t.Run("claude_provider_v1 present, job capability missing: not claimable", func(t *testing.T) {
		p := newPJobFix(t)
		r := p.mustCreate()
		w := p.je.seedWorkerRow(t, p.userID, false, nil, pjobNoJobCapCaps...)
		if _, err := p.tryClaim(w, false, pjobNoJobCapCaps); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("a worker without claude_provider_job_v1 claimed a provider job (err=%v)", err)
		}
		if n := p.claimable(r.ID); n != 0 {
			t.Fatalf("claimable workers = %d, want 0", n)
		}
		if !p.inEphemeralGap(r.ID) {
			t.Fatal("the job must be ephemeral demand while only a claude_provider_v1 worker is online")
		}
	})
	t.Run("the job capability without claude_provider_v1: not claimable", func(t *testing.T) {
		p := newPJobFix(t)
		r := p.mustCreate()
		w := p.je.seedWorkerRow(t, p.userID, false, nil, pjobNoProviderCaps...)
		if _, err := p.tryClaim(w, false, pjobNoProviderCaps); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("a worker without claude_provider_v1 claimed a provider job (err=%v)", err)
		}
		if n := p.claimable(r.ID); n != 0 {
			t.Fatalf("claimable workers = %d, want 0", n)
		}
	})
	t.Run("positive control: one fully capable non-Docker worker", func(t *testing.T) {
		p := newPJobFix(t)
		r := p.mustCreate()
		w := p.je.seedWorkerRow(t, p.userID, false, nil, pjobFullWorkerCaps...)
		if n := p.claimable(r.ID); n != 1 {
			t.Fatalf("claimable workers = %d, want 1", n)
		}
		if p.inEphemeralGap(r.ID) {
			t.Fatal("a job a fleet worker can serve must not be ephemeral demand")
		}
		run, err := p.tryClaim(w, false, pjobFullWorkerCaps)
		if err != nil || run.ID != r.ID || run.Status != "claimed" {
			t.Fatalf("ClaimRun = (%v %s, %v), want the job claimed", run.ID, run.Status, err)
		}
	})
	t.Run("peer mirror: a busy capable claimant does not defer to an idle peer lacking only the job capability", func(t *testing.T) {
		p := newPJobFix(t)
		r := p.mustCreate()
		cap2 := int32(2)
		me := p.je.seedWorkerRow(t, p.userID, false, &cap2, pjobFullWorkerCaps...)
		p.je.seedRawJob(t, p.userID, "running", &me, 30*time.Second, 600) // one active run: not minimum-loaded
		p.je.seedWorkerRow(t, p.userID, false, &cap2, pjobNoJobCapCaps...)
		run, err := p.tryClaim(me, false, pjobFullWorkerCaps)
		if err != nil || run.ID != r.ID {
			t.Fatalf("the busy claimant must claim: the peer (claude_provider_v1 without claude_provider_job_v1) can never take a provider job (run=%v err=%v)", run.ID, err)
		}
	})
	t.Run("peer mirror: a fully capable idle peer is a spread target", func(t *testing.T) {
		p := newPJobFix(t)
		r := p.mustCreate()
		cap2 := int32(2)
		me := p.je.seedWorkerRow(t, p.userID, false, &cap2, pjobFullWorkerCaps...)
		p.je.seedRawJob(t, p.userID, "running", &me, 30*time.Second, 600)
		p.je.seedWorkerRow(t, p.userID, false, &cap2, pjobFullWorkerCaps...)
		if _, err := p.tryClaim(me, false, pjobFullWorkerCaps); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("the busy claimant must defer to the capable idle peer (err=%v)", err)
		}
		if got := mustRun(t, p.env, r.ID).Status; got != "queued" {
			t.Fatalf("deferred job status = %s, want queued", got)
		}
	})
	t.Run("vendor controls: a vendor job is unchanged by the provider clauses", func(t *testing.T) {
		p := newPJobFix(t)
		p.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, p.userID)
		r := p.mustCreate()
		p.wantVendorJob(r)
		w := p.je.seedWorkerRow(t, p.userID, false, nil, pjobOldWorkerCaps...)
		if n := p.claimable(r.ID); n != 1 {
			t.Fatalf("claimable workers = %d, want the old job worker to count for a vendor job", n)
		}
		if p.inEphemeralGap(r.ID) {
			t.Fatal("a vendor job an old job worker can serve must not be ephemeral demand")
		}
		if run, err := p.tryClaim(w, false, pjobOldWorkerCaps); err != nil || run.ID != r.ID {
			t.Fatalf("the old job worker must claim the vendor job (run=%v err=%v)", run.ID, err)
		}
	})
}

// TestProviderJobQueuedReasonsLiveDB: the health rungs name the missing capability, and a vendor job
// never gets the provider wording.
func TestProviderJobQueuedReasonsLiveDB(t *testing.T) {
	row := func(p *pjobFix, provider bool) store.ListActiveRunsForHealthRow {
		r := store.ListActiveRunsForHealthRow{
			ID: uuid.New(), UserID: p.userID, Kind: runkind.Job, Status: "queued", Health: healthOK, Harness: harnessClaude,
			JobProtocol: pgtype.Int2{Int16: capability.JobProtocolFiles, Valid: true},
		}
		if provider {
			r.ModelProviderKeyMode = pgtype.Text{String: providerKeyModeUser, Valid: true}
		}
		return r
	}
	reason := func(p *pjobFix, provider bool) string {
		return p.svc.queuedReason(p.env.ctx, time.Now(), row(p, provider), thWithLimit(testCustodyLimit))
	}
	for _, tc := range []struct {
		name  string
		seed  func(p *pjobFix)
		want  string // "" = neither provider-job nor job-runner wording
		avoid []string
	}{
		{"only an old job worker: the provider rung names the job capability", func(p *pjobFix) { p.je.seedWorkerRow(t, p.userID, false, nil, pjobOldWorkerCaps...) }, reasonNoProviderJobCapableWorker, nil},
		{"claude_provider_v1 without the job capability", func(p *pjobFix) { p.je.seedWorkerRow(t, p.userID, false, nil, pjobNoJobCapCaps...) }, reasonNoProviderJobCapableWorker, nil},
		{"a Docker worker with every capability and no non-Docker job runner: the job rung", func(p *pjobFix) { p.je.seedWorkerRow(t, p.userID, true, nil, pjobFullWorkerCaps...) }, reasonNoJobCapableWorker, nil},
		{"a Docker full worker and a non-Docker old job worker: the job rung names the provider capability", func(p *pjobFix) {
			p.je.seedWorkerRow(t, p.userID, true, nil, pjobFullWorkerCaps...)
			p.je.seedWorkerRow(t, p.userID, false, nil, pjobOldWorkerCaps...)
		}, reasonNoProviderJobCapableWorker, nil},
		{"a fully capable non-Docker worker: no capability reason", func(p *pjobFix) { p.je.seedWorkerRow(t, p.userID, false, nil, pjobFullWorkerCaps...) }, "", []string{reasonNoProviderJobCapableWorker, reasonNoJobCapableWorker, reasonNoProviderCapableWorker}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPJobFix(t)
			tc.seed(p)
			got := reason(p, true)
			if tc.want != "" && got != tc.want {
				t.Fatalf("queuedReason = %q, want %q", got, tc.want)
			}
			for _, a := range tc.avoid {
				if got == a {
					t.Fatalf("queuedReason = %q, want none of the capability reasons", got)
				}
			}
		})
	}
	t.Run("a vendor job never gets the provider wording", func(t *testing.T) {
		p := newPJobFix(t)
		p.je.seedWorkerRow(t, p.userID, false, nil, pjobOldWorkerCaps...)
		if got := reason(p, false); got == reasonNoProviderJobCapableWorker || got == reasonNoProviderCapableWorker || got == reasonNoJobCapableWorker {
			t.Fatalf("a vendor job with an old job worker online got %q", got)
		}
	})
}

// TestProviderJobUnservableEphemeralLiveDB: a run-bound ephemeral worker that registered without the
// provider-job capabilities can never serve a provider job, so the D-A2 pass fails the job with
// no_job_capable_worker (the reason text names the provider capabilities); one that registered with
// every capability, and a vendor job's old-image worker, are left alone.
func TestProviderJobUnservableEphemeralLiveDB(t *testing.T) {
	const deadline = 10 * time.Minute
	for _, tc := range []struct {
		name string
		caps []string
		fail bool
	}{
		{"missing claude_provider_v1", pjobNoProviderCaps, true},
		{"missing claude_provider_job_v1", pjobNoJobCapCaps, true},
		{"job runner only", []string{jobCap, jobFilesCap}, true},
		{"every capability", pjobFullWorkerCaps, false},
	} {
		t.Run("provider job: "+tc.name, func(t *testing.T) {
			p := newPJobFix(t)
			r := p.mustCreate()
			w := p.je.seedEphemeralWorker(t, p.userID, r.ID)
			p.je.registerEphemeral(t, w, tc.caps...)
			n, err := p.svc.FailJobsWithUnservableEphemeral(p.env.ctx, deadline)
			if err != nil {
				t.Fatal(err)
			}
			got := mustRun(t, p.env, r.ID)
			if tc.fail {
				if n != 1 || got.Status != "failed" || got.FailOrigin.String != "no_job_capable_worker" ||
					!containsAll(got.FailureReason.String, capability.ClaudeProviderV1, capability.ClaudeProviderJobV1) {
					t.Fatalf("pass=%d job=%s/%q reason=%q, want failed/no_job_capable_worker naming the provider capabilities", n, got.Status, got.FailOrigin.String, got.FailureReason.String)
				}
				if p.je.workerExists(t, w.ID) {
					t.Fatal("the unservable ephemeral worker row must be deleted")
				}
				return
			}
			if n != 0 || got.Status != "queued" || !p.je.workerExists(t, w.ID) {
				t.Fatalf("pass=%d job=%s: a fully capable ephemeral worker must be left alone", n, got.Status)
			}
		})
	}
	t.Run("vendor job: an old-image job worker is left alone", func(t *testing.T) {
		p := newPJobFix(t)
		p.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, p.userID)
		r := p.mustCreate()
		w := p.je.seedEphemeralWorker(t, p.userID, r.ID)
		p.je.registerEphemeral(t, w, pjobOldWorkerCaps...)
		if n, err := p.svc.FailJobsWithUnservableEphemeral(p.env.ctx, deadline); err != nil || n != 0 {
			t.Fatalf("pass = %d, %v; want 0 (the vendor job's worker is capable)", n, err)
		}
		if got := mustRun(t, p.env, r.ID).Status; got != "queued" {
			t.Fatalf("vendor job status = %s, want queued", got)
		}
	})
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// pjobHibFix is a hibernation fixture whose owner has a usable Claude provider source.
func pjobHibFix(t *testing.T) (*hibFx, uuid.UUID, uuid.UUID) {
	t.Helper()
	f := newHibFx(t)
	f.svc.p.ProviderAllowedBaseURLs = []string{pvBase}
	provider, key := seedProviderFor(t, f.env, f.user, pvDefaultMap)
	// Registered after the fixture's user cleanup, so it runs first: a selected provider cannot be
	// deleted (RESTRICT) and the user delete cascades to its providers.
	t.Cleanup(func() {
		if _, err := f.env.pool.Exec(context.Background(), `UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, f.user); err != nil {
			t.Errorf("clear the Claude source in cleanup: %v", err)
		}
	})
	f.env.exec(`UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, f.user, provider)
	// The fixture's own infra worker must not suppress demand or stand in for a wake target.
	f.env.exec(`UPDATE workers SET status = 'offline' WHERE user_id = $1`, f.user)
	return f, provider, key
}

func (f *hibFx) createProviderJob() store.Run {
	f.t.Helper()
	v, err := f.svc.CreateJobRun(f.env.ctx, jobReq(cliCaller(f.user)))
	if err != nil {
		f.t.Fatalf("CreateJobRun: %v", err)
	}
	f.t.Cleanup(func() { _, _ = f.env.pool.Exec(context.Background(), `DELETE FROM runs WHERE id = $1`, v.ID) })
	return mustRun(f.t, f.env, v.ID)
}

// TestProviderJobWakeCensusLiveDB: ListHibernationWakeDemand (the job term) and ListStaleHibernationWakes
// (the Claude arm) only treat a worker with claude_provider_job_v1 as able to serve a provider job.
func TestProviderJobWakeCensusLiveDB(t *testing.T) {
	t.Run("wake demand picks only a worker with the job capability", func(t *testing.T) {
		f, _, _ := pjobHibFix(t)
		r := f.createProviderJob()
		without := f.sleepingWorker()
		f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, without, pjobNoJobCapCaps)
		if got := wakeCandidate(t, f, r.ID); got != uuid.Nil {
			t.Fatalf("wake candidate = %s, want none while only a claude_provider_v1 worker sleeps", got)
		}
		with := f.sleepingWorker()
		f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, with, pjobFullWorkerCaps)
		if got := wakeCandidate(t, f, r.ID); got != with {
			t.Fatalf("wake candidate = %s, want the fully capable worker %s", got, with)
		}
	})
	t.Run("a stale wake is released once the worker lacks the job capability", func(t *testing.T) {
		f, _, _ := pjobHibFix(t)
		capable, lost := f.createProviderJob(), f.createProviderJob()
		w1, _ := f.worker()
		w2, _ := f.worker()
		f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, w1, pjobFullWorkerCaps)
		f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, w2, pjobNoJobCapCaps)
		f.env.exec(`INSERT INTO hibernation_wakes (run_id, worker_id, first_woken_at) VALUES ($1, $2, now()), ($3, $4, now())`, capable.ID, w1, lost.ID, w2)
		rows, err := f.env.q.ListStaleHibernationWakes(f.env.ctx, 10000)
		if err != nil {
			t.Fatal(err)
		}
		var sawLost bool
		for _, row := range rows {
			if row.RunID == capable.ID {
				t.Fatal("a wake on a fully capable worker was listed stale")
			}
			if row.RunID == lost.ID && row.WorkerID == w2 {
				sawLost = true
			}
		}
		if !sawLost {
			t.Fatal("a wake on a worker without claude_provider_job_v1 was not listed stale")
		}
		// Drive the real release: the lost wake row is deleted, the capable one stays.
		if released, err := f.svc.ReleaseStaleWake(f.env.ctx, lost.ID); err != nil || !released {
			t.Fatalf("ReleaseStaleWake(lost) = %v, %v; want released", released, err)
		}
		if n := f.wakeRows(lost.ID); n != 0 {
			t.Fatalf("lost run still has %d wake rows after release", n)
		}
		if released, err := f.svc.ReleaseStaleWake(f.env.ctx, capable.ID); err != nil || released {
			t.Fatalf("ReleaseStaleWake(capable) = %v, %v; want kept", released, err)
		}
		if n := f.wakeRows(capable.ID); n != 1 {
			t.Fatalf("capable run has %d wake rows, want 1", n)
		}
	})
}
