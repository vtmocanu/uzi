package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ci_config_guard_claim_livedb_test.go pins the ci_config_guard_v1 claim gate (PRD #2865) on
// the real claim path: a run carrying runs.ci_config_guard is claimable only by a worker
// self-reporting the protocol capability, whatever the kill-switch, the run's required
// capabilities or the claimant's other reports say. Each refusal also asserts the run is still
// queued and unowned. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// guardWorker seeds an online, freshly-heartbeating persistent worker for the fixture user
// reporting the given protocol capabilities.
func (f *crFix) guardWorker(t *testing.T, maxRuns int, proto ...string) store.Worker {
	t.Helper()
	id := uuid.New()
	a, b := uuid.New(), uuid.New()
	if proto == nil {
		proto = []string{}
	}
	var max pgtype.Int4
	if maxRuns > 0 {
		max = pgtype.Int4{Int32: int32(maxRuns), Valid: true} //nolint:gosec // G115: a small test constant
	}
	f.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, protocol_capabilities, max_concurrent_runs)
	            VALUES ($1, $2, $3, $4, 'online', now(), $5, $6)`, id, f.user, "guard-"+id.String(), append(a[:], b[:]...), proto, max)
	w, err := f.env.q.GetWorkerByID(f.env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// startGuardedRun creates the guarded repair run (left queued) and returns its id.
func (f *crFix) startGuardedRun(t *testing.T, key string, id int64) uuid.UUID {
	t.Helper()
	run, err := f.svc.CreateCIRepairRun(f.env.ctx, f.params(t, key, crObs(id, int(id), int(id)), "S"+key, 3))
	if err != nil {
		t.Fatal(err)
	}
	return run.ID
}

// startPlainPromptRun creates an unguarded scheduled prompt run (left queued).
func (f *crFix) startPlainPromptRun(t *testing.T) uuid.UUID {
	t.Helper()
	other, err := f.env.q.CreateRunSchedule(f.env.ctx, store.CreateRunScheduleParams{
		UserID: f.user, RepoID: f.repo, Target: "prompt", Prompt: pgtype.Text{String: "p", Valid: true}, Timing: "recurring",
		CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, Timezone: "UTC", Enabled: true,
		NextFireAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.svc.CreatePromptRun(f.env.ctx, f.user, f.repo, other.ID, "t", "p", false, false, nil, nil, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return run.ID
}

func (f *crFix) refuseClaim(t *testing.T, w store.Worker, run uuid.UUID, why string) {
	t.Helper()
	p, err := f.svc.Claim(f.env.ctx, w, nil)
	if err != nil {
		t.Fatalf("%s: Claim: %v", why, err)
	}
	if p != nil {
		t.Fatalf("%s: claimed run %s, want idle", why, p.RunID)
	}
	var status string
	var owner pgtype.UUID
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status, worker_id FROM runs WHERE id = $1`, run).Scan(&status, &owner); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || owner.Valid {
		t.Fatalf("%s: run is %s owned=%v, want queued and unowned", why, status, owner.Valid)
	}
}

func (f *crFix) acceptClaim(t *testing.T, w store.Worker, run uuid.UUID, why string) *ClaimPayload {
	t.Helper()
	p, err := f.svc.Claim(f.env.ctx, w, nil)
	if err != nil {
		t.Fatalf("%s: Claim: %v", why, err)
	}
	if p == nil || p.RunID != run.String() {
		t.Fatalf("%s: Claim = %+v, want run %s", why, p, run)
	}
	return p
}

// TestCIConfigGuardClaimGateLiveDB: an old worker can never claim a guarded run, with the
// capability-aware kill-switch off and with the run's required capabilities cleared; a worker
// reporting ci_config_guard_v1 does, and the claim carries the guard flag and the floor paths.
func TestCIConfigGuardClaimGateLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*crFix, uuid.UUID)
	}{
		{"default", func(*crFix, uuid.UUID) {}},
		{"capability_aware off", func(f *crFix, _ uuid.UUID) { f.svc.capabilitySettings = fakeCapabilitySettings{on: false} }},
		{"cleared required_capabilities", func(f *crFix, run uuid.UUID) {
			f.env.exec(`UPDATE runs SET required_capabilities = '{docker}' WHERE id = $1`, run)
			f.env.exec(`UPDATE runs SET required_capabilities = '{}' WHERE id = $1`, run)
		}},
		{"old worker reporting every other protocol capability", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCRFix(t)
			run := f.startGuardedRun(t, "k", 100)
			var old store.Worker
			if tc.setup != nil {
				tc.setup(f, run)
				old = f.guardWorker(t, 0)
			} else {
				old = f.guardWorker(t, 0, capability.CompletionInterlockV1, capability.CodexHarnessV1, capability.CodexRuntimeV2,
					capability.RecoveryArchiveV1, capability.CrossCheckV1, capability.JobRunnerV1)
			}
			f.refuseClaim(t, old, run, "worker without ci_config_guard_v1")

			capable := f.guardWorker(t, 0, capability.CIConfigGuardV1)
			p := f.acceptClaim(t, capable, run, "worker with ci_config_guard_v1")
			if !p.Config.CIConfigGuard {
				t.Fatal("the claim of a guarded run must carry ci_config_guard")
			}
			for _, want := range CIRepairConfigFloor {
				found := false
				for _, got := range p.Config.CIConfigPaths {
					found = found || got == want
				}
				if !found {
					t.Fatalf("claim paths %v lack the floor entry %q", p.Config.CIConfigPaths, want)
				}
			}
		})
	}
}

// TestUnguardedPromptRunStaysClaimableLiveDB: the gate keys on the run's flag, so an ordinary
// prompt run is still claimable by a worker without the capability.
func TestUnguardedPromptRunStaysClaimableLiveDB(t *testing.T) {
	f := newCRFix(t)
	run := f.startPlainPromptRun(t)
	old := f.guardWorker(t, 0)
	p := f.acceptClaim(t, old, run, "old worker, unguarded prompt run")
	if p.Config.CIConfigGuard || p.Config.CIConfigPaths != nil {
		t.Fatalf("unguarded claim carries guard=%v paths=%v", p.Config.CIConfigGuard, p.Config.CIConfigPaths)
	}
}

// TestCIConfigGuardPeerDeferralLiveDB: the fleet-spread peer arm mirrors the gate. A capable
// but busy claimant must not defer a guarded run to an idle peer that can never claim it.
func TestCIConfigGuardPeerDeferralLiveDB(t *testing.T) {
	f := newCRFix(t)
	capable := f.guardWorker(t, 4, capability.CIConfigGuardV1)
	f.guardWorker(t, 4) // an idle old peer: it could never claim the guarded run
	busy := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	            VALUES ($1, $2, $3, 'issue', 771, 't', 'd', 'running', $4)`, busy, f.user, f.repo, capable.ID)
	run := f.startGuardedRun(t, "k", 100)
	// The run is fresh (younger than the spread cutoff), so the fleet-aware spread is live: a
	// busy claimant defers to a strictly less loaded eligible peer unless the peer arm excludes it.
	f.acceptClaim(t, capable, run, "busy capable worker with an idle incapable peer")
}

// TestCIConfigGuardPlacementMirrorsLiveDB: the claimable count, the capability-gap list and
// the saturation list all treat a worker without the capability as unable to serve a guarded
// run, so ephemeral demand sees the real gap.
func TestCIConfigGuardPlacementMirrorsLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	f.env.exec(`UPDATE users SET ephemeral_workers_enabled = true WHERE id = $1`, f.user)
	run := f.startGuardedRun(t, "k", 100)
	f.env.exec(`UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, run)

	now := time.Now()
	hb := pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true}
	cutoff := pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true}
	claimable := func() int64 {
		t.Helper()
		n, err := f.env.q.CountOnlineWorkersClaimableForRun(ctx, store.CountOnlineWorkersClaimableForRunParams{
			RunID: run, HeartbeatCutoff: hb, AffinityCutoff: cutoff, CrossCheckEvaluatedAt: cutoff, CrossCheckAffinityCutoff: cutoff,
			CapabilityAware: true, CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{},
			EphemeralLease: pgtype.Interval{Valid: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		return n.Claimable
	}
	gap := func() bool {
		t.Helper()
		rows, err := f.env.q.ListUnplaceableQueuedRunsForEphemeral(ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
			CrossCheckEvaluatedAt: cutoff, CrossCheckAffinityCutoff: cutoff, BackgroundGraceCutoff: cutoff,
			CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true},
			MaxPerUser: 1000, MaxRows: 10000,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == run {
				return true
			}
		}
		return false
	}
	saturated := func() bool {
		t.Helper()
		rows, err := f.env.q.ListSaturationQueuedRunsForEphemeral(ctx, store.ListSaturationQueuedRunsForEphemeralParams{
			CrossCheckEvaluatedAt: cutoff, CrossCheckAffinityCutoff: cutoff, BackgroundGraceCutoff: cutoff,
			CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true},
			SaturationDelay: pgtype.Interval{Valid: true}, MaxPerUser: 1000, MaxRows: 10000,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == run {
				return true
			}
		}
		return false
	}

	// Only an old worker: nobody can claim it, the gap lists it, saturation must not.
	f.guardWorker(t, 0)
	if n := claimable(); n != 0 {
		t.Fatalf("claimable with only an old worker = %d, want 0", n)
	}
	if !gap() {
		t.Fatal("a guarded run with only old workers online must be a capability-gap candidate")
	}
	if saturated() {
		t.Fatal("a guarded run with only old workers online is a gap, not slot saturation")
	}

	// A capable worker with room is claimable; the old worker never counts.
	capable := f.guardWorker(t, 1, capability.CIConfigGuardV1)
	if n := claimable(); n != 1 {
		t.Fatalf("claimable with one capable worker = %d, want 1", n)
	}
	if gap() {
		t.Fatal("a capable worker is online: not a capability gap")
	}
	if saturated() {
		t.Fatal("a capable worker has room: not slot saturation")
	}

	// Once the capable worker is full, the run is saturated (capable workers have no room),
	// and the idle unbounded old worker is not a free slot for it.
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	            VALUES ($1, $2, $3, 'issue', 772, 't', 'd', 'running', $4)`, uuid.New(), f.user, f.repo, capable.ID)
	if n := claimable(); n != 0 {
		t.Fatalf("claimable with the capable worker full = %d, want 0 (the old worker cannot serve it)", n)
	}
	if gap() {
		t.Fatal("a capable worker is online: not a capability gap")
	}
	if !saturated() {
		t.Fatal("capable workers are full and the old worker cannot serve the run: it must be listed as saturated")
	}
}
