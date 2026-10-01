package workersvc

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These cases exercise the Register transaction at the requeue limit. They supply
// snapshot and lease inputs directly; agent-side tests establish the loaded-journal
// overflow snapshot, while these seeded lease rows test SQL guards independently.
// No heartbeat or boot replay can change the transaction result before assertion.
func TestRegisterPendingOutcomeClassificationLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name            string
		snapshot        func(uuid.UUID) *ActiveSnapshot
		wantStatus      string
		wantOrigin      string
		wantLease       bool
		wantOverflow    bool
		preseedLeaseGen int64
	}{
		{
			name:       "absent register snapshot fails as worker lost",
			wantStatus: "failed", wantOrigin: "worker_lost",
		},
		{
			name: "valid worker register overflow snapshot preserves run",
			snapshot: func(_ uuid.UUID) *ActiveSnapshot {
				return &ActiveSnapshot{SnapshotEpoch: 0, PendingOverflow: true, Active: []ActiveRunEntry{}}
			},
			wantStatus: "running", wantOverflow: true,
		},
		{
			name:            "prior exact-generation terminal lease protects run without register snapshot",
			preseedLeaseGen: 1,
			wantStatus:      "running", wantLease: true,
		},
		{
			name:            "wrong-generation terminal lease cannot protect run without register snapshot",
			preseedLeaseGen: 2,
			wantStatus:      "failed", wantOrigin: "worker_lost", wantLease: true,
		},
		{
			name: "invalid register snapshot is ignored and orphan pass fails worker lost",
			snapshot: func(run uuid.UUID) *ActiveSnapshot {
				return &ActiveSnapshot{SnapshotEpoch: 0, PendingOverflow: true,
					Active: []ActiveRunEntry{entry(run, 1, "invalid_phase", true)}}
			},
			wantStatus: "failed", wantOrigin: "worker_lost",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, _, repoID := env.seedCodexInfra(t)
			svc := snapshotSvc(env, testParams())
			workerID := seedSnapshotWorker(t, env, userID, "old-nonce")
			runID := seedOutageRun(t, env, userID, repoID, workerID, "running", "issue", 1, 1)
			holdID := claimRecoveryHold(t, env, runID, workerID, 1)
			if tc.preseedLeaseGen != 0 {
				insertActiveLease(t, env, workerID, runID, tc.preseedLeaseGen, true, "1 hour")
			}

			var snap *ActiveSnapshot
			if tc.snapshot != nil {
				snap = tc.snapshot(runID)
			}
			if _, _, err := svc.Register(env.ctx, store.Worker{ID: workerID, UserID: userID},
				"v1", "base", nil, nil, nil, snap); err != nil {
				t.Fatalf("Register: %v", err)
			}

			var status string
			var generation int64
			var requeues int32
			if err := env.pool.QueryRow(env.ctx,
				`SELECT status, claim_generation, requeue_count FROM runs WHERE id = $1`, runID).
				Scan(&status, &generation, &requeues); err != nil {
				t.Fatalf("read run: %v", err)
			}
			if status != tc.wantStatus || generation != 1 || requeues != 1 {
				t.Fatalf("run status=%q generation=%d requeues=%d, want %q/1/1",
					status, generation, requeues, tc.wantStatus)
			}
			if origin := failOriginOf(t, env, runID); origin != tc.wantOrigin {
				t.Fatalf("fail origin = %q, want %q", origin, tc.wantOrigin)
			}
			lease, ok := readActiveRun(t, env, workerID, runID)
			if ok != tc.wantLease {
				t.Fatalf("active lease present = %v, want %v", ok, tc.wantLease)
			}
			if tc.wantLease && (lease.gen != tc.preseedLeaseGen || !lease.terminalPending || !lease.untilFuture) {
				t.Fatalf("active lease = %+v, want future terminal lease at generation %d", lease, tc.preseedLeaseGen)
			}
			if flag, future := workerOverflow(t, env, workerID); flag != tc.wantOverflow || future != tc.wantOverflow {
				t.Fatalf("overflow flag=%v future=%v, want both %v", flag, future, tc.wantOverflow)
			}
			var holdGeneration int64
			var holdState string
			if err := env.pool.QueryRow(env.ctx,
				`SELECT generation, state FROM recovery_custody_holds WHERE id = $1 AND run_id = $2 AND live_worker_id = $3`,
				holdID, runID, workerID).Scan(&holdGeneration, &holdState); err != nil {
				t.Fatalf("read exact custody hold: %v", err)
			}
			if holdGeneration != generation || holdState != "open" {
				t.Fatalf("custody hold generation=%d state=%q, want %d/open", holdGeneration, holdState, generation)
			}
		})
	}
}

// ---- Attested finalize-resume (issue #1742) ---------------------------------------------------

// finalizeSnap builds a register snapshot that offers the given (run, generation) attestations.
func finalizeSnap(overflow bool, pairs ...FinalizeResumeEntry) *ActiveSnapshot {
	return &ActiveSnapshot{SnapshotEpoch: 0, PendingOverflow: overflow, Active: []ActiveRunEntry{}, FinalizeResume: pairs}
}

func fin(run uuid.UUID, gen int64) FinalizeResumeEntry {
	return FinalizeResumeEntry{RunID: run.String(), ClaimGeneration: gen}
}

// finalizeMarkOf reads runs.finalize_resume_generation (ok=false when NULL).
func finalizeMarkOf(t *testing.T, env codexTestEnv, id uuid.UUID) (int64, bool) {
	t.Helper()
	var v *int64
	if err := env.pool.QueryRow(env.ctx, `SELECT finalize_resume_generation FROM runs WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read finalize_resume_generation: %v", err)
	}
	if v == nil {
		return 0, false
	}
	return *v, true
}

func registerWith(t *testing.T, env codexTestEnv, svc *Service, userID, workerID uuid.UUID, snap *ActiveSnapshot) {
	t.Helper()
	if _, _, err := svc.Register(env.ctx, store.Worker{ID: workerID, UserID: userID}, "v1", "base", nil, nil, nil, snap); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

// assertNotRunningUnder is the #1742 invariant on the ordinary path: after an accepted Register
// no offered run remains status 'running' under this worker.
func assertNotRunningUnder(t *testing.T, env codexTestEnv, workerID uuid.UUID, runs ...uuid.UUID) {
	t.Helper()
	for _, r := range runs {
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM runs WHERE id = $1 AND status = 'running' AND worker_id = $2`, r, workerID).Scan(&n); err != nil {
			t.Fatalf("invariant read: %v", err)
		}
		if n != 0 {
			t.Fatalf("invariant broken: offered run %s is still running under worker %s after Register", r, workerID)
		}
	}
}

// claimFor claims the next queued run for the worker through the real ClaimRun path.
func claimFor(t *testing.T, env codexTestEnv, workerID uuid.UUID) (store.Run, error) {
	t.Helper()
	return env.q.ClaimRun(env.ctx, claimRunParamsFor(hbWorker(t, env, workerID)))
}

func TestRegisterAttestedFinalizeLiveDB(t *testing.T) {
	type want struct {
		status    string
		origin    string
		requeues  int32
		mark      int64
		hasMark   bool
		overflow  bool
		claimable bool // after Register, ClaimRun for the same worker returns the run
	}
	for _, tc := range []struct {
		name string
		// seed
		gen         int64
		requeues    int32
		priorMark   int64 // 0 = NULL
		maxRequeues int
		// offered attestation generation (-1 = the seeded generation)
		offerGen int64
		overflow bool
		lease    bool
		want     want
	}{
		{name: "at the limit uses the one-shot allowance", gen: 1, requeues: 1, maxRequeues: 1, offerGen: -1,
			want: want{status: "queued", requeues: 2, mark: 1, hasMark: true, claimable: true}},
		{name: "second attested restart after the allowance is failed", gen: 2, requeues: 2, priorMark: 1, maxRequeues: 1, offerGen: -1,
			want: want{status: "failed", origin: "worker_lost", requeues: 2, mark: 1, hasMark: true}},
		{name: "under budget is an ordinary requeue without the mark", gen: 1, requeues: 0, maxRequeues: 1, offerGen: -1,
			want: want{status: "queued", requeues: 1, claimable: true}},
		{name: "RUN_MAX_REQUEUES=0 never re-queues", gen: 1, requeues: 0, maxRequeues: 0, offerGen: -1,
			want: want{status: "failed", origin: "worker_lost", requeues: 0}},
		{name: "overflow closure does not block the allowance", gen: 1, requeues: 1, maxRequeues: 1, offerGen: -1, overflow: true,
			want: want{status: "queued", requeues: 2, mark: 1, hasMark: true, overflow: true, claimable: false}},
		{name: "overflow closure does not block failing an ineligible attested run", gen: 2, requeues: 2, priorMark: 1, maxRequeues: 1, offerGen: -1, overflow: true,
			want: want{status: "failed", origin: "worker_lost", requeues: 2, mark: 1, hasMark: true, overflow: true}},
		{name: "wrong generation falls to the ordinary pass", gen: 1, requeues: 1, maxRequeues: 1, offerGen: 2,
			want: want{status: "failed", origin: "worker_lost", requeues: 1}},
		{name: "live exact-generation terminal lease keeps the run running", gen: 1, requeues: 1, maxRequeues: 1, offerGen: -1, lease: true,
			want: want{status: "running", requeues: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, _, repoID := env.seedCodexInfra(t)
			p := testParams()
			p.RunMaxRequeues = tc.maxRequeues
			svc := snapshotSvc(env, p)
			workerID := seedSnapshotWorker(t, env, userID, "old-nonce")
			runID := seedOutageRun(t, env, userID, repoID, workerID, "running", "issue", tc.gen, tc.requeues)
			if tc.priorMark != 0 {
				env.exec(`UPDATE runs SET finalize_resume_generation = $2 WHERE id = $1`, runID, tc.priorMark)
			}
			if tc.lease {
				insertActiveLease(t, env, workerID, runID, tc.gen, true, "1 hour")
			}
			offer := tc.offerGen
			if offer < 0 {
				offer = tc.gen
			}
			registerWith(t, env, svc, userID, workerID, finalizeSnap(tc.overflow, fin(runID, offer)))

			if got := statusOf(t, env, runID); got != tc.want.status {
				t.Fatalf("status = %q, want %q", got, tc.want.status)
			}
			if got := requeueCountOf(t, env, runID); got != int(tc.want.requeues) {
				t.Fatalf("requeue_count = %d, want %d", got, tc.want.requeues)
			}
			if got := failOriginOf(t, env, runID); got != tc.want.origin {
				t.Fatalf("fail_origin = %q, want %q", got, tc.want.origin)
			}
			mark, hasMark := finalizeMarkOf(t, env, runID)
			if hasMark != tc.want.hasMark || mark != tc.want.mark {
				t.Fatalf("finalize_resume_generation = (%d,%v), want (%d,%v)", mark, hasMark, tc.want.mark, tc.want.hasMark)
			}
			if flag, future := workerOverflow(t, env, workerID); flag != tc.want.overflow || future != tc.want.overflow {
				t.Fatalf("overflow flag=%v future=%v, want both %v", flag, future, tc.want.overflow)
			}
			if !tc.lease {
				assertNotRunningUnder(t, env, workerID, runID)
			}
			if tc.want.status == "queued" {
				claimed, err := claimFor(t, env, workerID)
				switch {
				case tc.want.claimable:
					if err != nil || claimed.ID != runID || claimed.ClaimGeneration != tc.gen+1 {
						t.Fatalf("ClaimRun = %s gen %d err=%v, want run %s at generation %d", claimed.ID, claimed.ClaimGeneration, err, runID, tc.gen+1)
					}
				case !errors.Is(err, pgx.ErrNoRows):
					t.Fatalf("ClaimRun during the overflow closure = %s err=%v, want no claimable run", claimed.ID, err)
				}
			}
		})
	}
}

// TestRegisterAttestedFinalizeIgnoresForeignRunLiveDB: a run owned by ANOTHER worker named in the
// list is untouched.
func TestRegisterAttestedFinalizeIgnoresForeignRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())
	me := seedSnapshotWorker(t, env, userID, "old-nonce")
	other := seedSnapshotWorker(t, env, userID, "other-nonce")
	foreign := seedOutageRun(t, env, userID, repoID, other, "running", "issue", 1, 1)
	registerWith(t, env, svc, userID, me, finalizeSnap(false, fin(foreign, 1)))
	if got := statusOf(t, env, foreign); got != "running" {
		t.Fatalf("foreign run status = %q, want untouched running", got)
	}
	if got := requeueCountOf(t, env, foreign); got != 1 {
		t.Fatalf("foreign run requeue_count = %d, want 1", got)
	}
	if _, has := finalizeMarkOf(t, env, foreign); has {
		t.Fatal("foreign run carries a finalize mark")
	}
}

// TestRegisterFinalizeValidatedIndependentlyLiveDB: an invalid Active list does not drop a valid
// finalize list, and an invalid finalize list does not drop a valid Active list.
func TestRegisterFinalizeValidatedIndependentlyLiveDB(t *testing.T) {
	t.Run("invalid active, valid finalize still applies", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, _, repoID := env.seedCodexInfra(t)
		svc := snapshotSvc(env, testParams())
		wk := seedSnapshotWorker(t, env, userID, "old-nonce")
		run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 1)
		snap := &ActiveSnapshot{PendingOverflow: true,
			Active:         []ActiveRunEntry{entry(run, 1, "invalid_phase", true)},
			FinalizeResume: []FinalizeResumeEntry{fin(run, 1)}}
		registerWith(t, env, svc, userID, wk, snap)
		if got := statusOf(t, env, run); got != "queued" {
			t.Fatalf("status = %q, want queued (finalize list applied)", got)
		}
		if _, ok := readActiveRun(t, env, wk, run); ok {
			t.Fatal("invalid Active list was applied")
		}
	})
	t.Run("invalid finalize, valid active still applies", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, _, repoID := env.seedCodexInfra(t)
		svc := snapshotSvc(env, testParams())
		wk := seedSnapshotWorker(t, env, userID, "old-nonce")
		attested := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 1)
		journaled := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 1)
		snap := &ActiveSnapshot{
			Active:         []ActiveRunEntry{entry(journaled, 1, "running", true)},
			FinalizeResume: []FinalizeResumeEntry{fin(attested, 1), fin(attested, 1)}} // duplicate: dropped
		registerWith(t, env, svc, userID, wk, snap)
		if lease, ok := readActiveRun(t, env, wk, journaled); !ok || !lease.terminalPending {
			t.Fatalf("valid Active list was not applied: lease=%+v ok=%v", lease, ok)
		}
		if got := statusOf(t, env, journaled); got != "running" {
			t.Fatalf("journaled run status = %q, want running (lease-protected)", got)
		}
		// The dropped finalize list means today's behaviour for the attested run: over budget, failed
		// by the ordinary pass, and no allowance mark.
		if got := statusOf(t, env, attested); got != "failed" {
			t.Fatalf("attested run status = %q, want failed via the ordinary pass", got)
		}
		if _, has := finalizeMarkOf(t, env, attested); has {
			t.Fatal("dropped finalize list still stamped the allowance mark")
		}
	})
}

// TestRegisterFinalizeKnownLimitFallsBackToMissingPathLiveDB pins the documented known limit: a
// register whose overflow snapshot carries an ABSENT or INVALID (dropped) finalize list leaves G
// running; after the closure lapses a heartbeat snapshot omitting G goes through the ordinary
// missing-from-snapshot reconciliation, exactly as on main.
func TestRegisterFinalizeKnownLimitFallsBackToMissingPathLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name       string
		list       func(run uuid.UUID) []FinalizeResumeEntry
		requeues   int32
		wantStatus string
		wantOrigin string
	}{
		{"absent list, over cap", func(uuid.UUID) []FinalizeResumeEntry { return nil }, 1, "failed", "worker_lost"},
		{"invalid list, over cap", func(r uuid.UUID) []FinalizeResumeEntry { return []FinalizeResumeEntry{fin(r, 1), fin(r, 1)} }, 1, "failed", "worker_lost"},
		{"absent list, under budget", func(uuid.UUID) []FinalizeResumeEntry { return nil }, 0, "queued", ""},
		{"invalid list, under budget", func(r uuid.UUID) []FinalizeResumeEntry { return []FinalizeResumeEntry{fin(r, 1), fin(r, 1)} }, 0, "queued", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, _, repoID := env.seedCodexInfra(t)
			svc := snapshotSvc(env, testParams())
			wk := seedSnapshotWorker(t, env, userID, "old-nonce")
			run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, tc.requeues)
			registerWith(t, env, svc, userID, wk, finalizeSnap(true, tc.list(run)...))
			if got := statusOf(t, env, run); got != "running" {
				t.Fatalf("after register status = %q, want running (overflow closure, no valid attestation)", got)
			}
			// Closure lapses; the run's status_since is already 3h old (past the missing fence).
			env.exec(`UPDATE workers SET pending_overflow_until = now() - interval '1 second' WHERE id = $1`, wk)
			w := hbWorker(t, env, wk)
			hb := &ActiveSnapshot{SnapshotEpoch: w.SnapshotEpoch + 1, RegisterNonce: w.SnapshotRegisterNonce.String, Active: []ActiveRunEntry{}}
			if _, err := svc.Heartbeat(env.ctx, w, nil, nil, hb); err != nil {
				t.Fatalf("Heartbeat: %v", err)
			}
			if got := statusOf(t, env, run); got != tc.wantStatus {
				t.Fatalf("after heartbeat status = %q, want %q", got, tc.wantStatus)
			}
			if got := failOriginOf(t, env, run); got != tc.wantOrigin {
				t.Fatalf("fail_origin = %q, want %q", got, tc.wantOrigin)
			}
		})
	}
}

// TestRegisterAttestedFinalizeCrossedPairingLiveDB: the pairing is positional. Run A at generation
// 1 and run B at generation 2 offered as A:2, B:1 are handled by NEITHER attested query (no mark, no
// allowance); the ordinary pass fails both (over budget). The correct two-pair offer handles both.
func TestRegisterAttestedFinalizeCrossedPairingLiveDB(t *testing.T) {
	setup := func(t *testing.T) (codexTestEnv, *Service, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) {
		env := setupCodexLiveDB(t)
		userID, _, repoID := env.seedCodexInfra(t)
		svc := snapshotSvc(env, testParams()) // RunMaxRequeues = 1
		wk := seedSnapshotWorker(t, env, userID, "old-nonce")
		a := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 1)
		b := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 2, 1)
		return env, svc, userID, wk, a, b
	}
	t.Run("crossed offer handles neither", func(t *testing.T) {
		env, svc, userID, wk, a, b := setup(t)
		registerWith(t, env, svc, userID, wk, finalizeSnap(false, fin(a, 2), fin(b, 1)))
		for _, r := range []uuid.UUID{a, b} {
			if got := statusOf(t, env, r); got != "failed" {
				t.Fatalf("run %s status = %q, want failed by the ordinary pass", r, got)
			}
			if got := failOriginOf(t, env, r); got != "worker_lost" {
				t.Fatalf("run %s fail_origin = %q, want worker_lost", r, got)
			}
			if got := requeueCountOf(t, env, r); got != 1 {
				t.Fatalf("run %s requeue_count = %d, want 1 (no allowance requeue)", r, got)
			}
			if _, has := finalizeMarkOf(t, env, r); has {
				t.Fatalf("run %s carries a finalize mark from a crossed pair", r)
			}
		}
	})
	t.Run("correct two-pair offer handles both", func(t *testing.T) {
		env, svc, userID, wk, a, b := setup(t)
		registerWith(t, env, svc, userID, wk, finalizeSnap(false, fin(a, 1), fin(b, 2)))
		for r, gen := range map[uuid.UUID]int64{a: 1, b: 2} {
			if got := statusOf(t, env, r); got != "queued" {
				t.Fatalf("run %s status = %q, want queued", r, got)
			}
			if mark, has := finalizeMarkOf(t, env, r); !has || mark != gen {
				t.Fatalf("run %s mark = (%d,%v), want (%d,true)", r, mark, has, gen)
			}
		}
	})
}

// TestRegisterAttestedFinalizeExcludedShapesLiveDB: shapes the attested queries must never touch,
// even listed at their exact generation. Each case seeds a target and an identical control that is
// not attested; the attested pass must leave the target exactly as the ordinary pass leaves the
// control (same status and requeue_count) and never stamp the allowance mark.
func TestRegisterAttestedFinalizeExcludedShapesLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, env codexTestEnv, userID, repoID, wk uuid.UUID) uuid.UUID
	}{
		{"chat run", func(t *testing.T, env codexTestEnv, userID, _, wk uuid.UUID) uuid.UUID {
			return seedOutageChatRun(t, env, userID, wk, 1, 1)
		}},
		{"paused run with a released claim", func(t *testing.T, env codexTestEnv, userID, repoID, wk uuid.UUID) uuid.UUID {
			r := seedOutageRun(t, env, userID, repoID, wk, "paused", "issue", 1, 1)
			env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, r)
			return r
		}},
		{"running run with a released claim", func(t *testing.T, env codexTestEnv, userID, repoID, wk uuid.UUID) uuid.UUID {
			r := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 1)
			env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, r)
			return r
		}},
		{"awaiting_approval run", func(t *testing.T, env codexTestEnv, userID, repoID, wk uuid.UUID) uuid.UUID {
			return seedOutageRun(t, env, userID, repoID, wk, "awaiting_approval", "issue", 1, 1)
		}},
		{"awaiting_input run", func(t *testing.T, env codexTestEnv, userID, repoID, wk uuid.UUID) uuid.UUID {
			return seedOutageRun(t, env, userID, repoID, wk, "awaiting_input", "issue", 1, 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, _, repoID := env.seedCodexInfra(t)
			svc := snapshotSvc(env, testParams())
			wk := seedSnapshotWorker(t, env, userID, "old-nonce")
			target := tc.seed(t, env, userID, repoID, wk)
			control := tc.seed(t, env, userID, repoID, wk)
			registerWith(t, env, svc, userID, wk, finalizeSnap(false, fin(target, 1)))
			if _, has := finalizeMarkOf(t, env, target); has {
				t.Fatal("attested pass stamped the allowance mark on an excluded shape")
			}
			if got, want := statusOf(t, env, target), statusOf(t, env, control); got != want {
				t.Fatalf("attested target status = %q, unattested control = %q: the attested pass transitioned it", got, want)
			}
			if got, want := requeueCountOf(t, env, target), requeueCountOf(t, env, control); got != want {
				t.Fatalf("attested target requeue_count = %d, control = %d", got, want)
			}
		})
	}
}

// TestRequeueAttestedFinalizeAllowanceUsedFlagLiveDB pins the AllowanceUsed column of the real
// query: true only when the one-shot allowance fired, false for an under-budget requeue.
func TestRequeueAttestedFinalizeAllowanceUsedFlagLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name          string
		requeues      int32
		wantAllowance bool
	}{
		{"over budget uses the allowance", 1, true},
		{"under budget is an ordinary requeue", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, _, repoID := env.seedCodexInfra(t)
			wk := seedSnapshotWorker(t, env, userID, "old-nonce")
			run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, tc.requeues)
			rows, err := env.q.RequeueAttestedFinalizeRuns(env.ctx, store.RequeueAttestedFinalizeRunsParams{
				MaxRequeues: 1, WorkerID: pgconv.UUID(wk), RunIds: []uuid.UUID{run}, ClaimGenerations: []int64{1},
			})
			if err != nil {
				t.Fatalf("RequeueAttestedFinalizeRuns: %v", err)
			}
			if len(rows) != 1 || rows[0].ID != run || rows[0].AllowanceUsed != tc.wantAllowance {
				t.Fatalf("rows = %+v, want one row for %s with AllowanceUsed=%v", rows, run, tc.wantAllowance)
			}
		})
	}
}

// TestRegisterMalformedFinalizeWireKeepsActiveLeaseLiveDB: a snapshot decoded from JSON whose
// finalize_resume has the wrong wire type still applies its valid Active terminal_pending lease, so
// the run stays protected (#1391) instead of being failed worker_lost.
func TestRegisterMalformedFinalizeWireKeepsActiveLeaseLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())
	wk := seedSnapshotWorker(t, env, userID, "old-nonce")
	run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 1)
	raw := `{"snapshot_epoch":0,"active":[{"run_id":"` + run.String() + `","claim_generation":1,"phase":"running","terminal_pending":true}],` +
		`"finalize_resume":[{"run_id":"` + run.String() + `","claim_generation":"1"}]}`
	var snap ActiveSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	registerWith(t, env, svc, userID, wk, &snap)
	if lease, ok := readActiveRun(t, env, wk, run); !ok || !lease.terminalPending {
		t.Fatalf("valid Active lease not applied: lease=%+v ok=%v", lease, ok)
	}
	if got := statusOf(t, env, run); got != "running" {
		t.Fatalf("status = %q, want running (lease-protected)", got)
	}
	if _, has := finalizeMarkOf(t, env, run); has {
		t.Fatal("a malformed finalize list stamped the allowance mark")
	}
}
