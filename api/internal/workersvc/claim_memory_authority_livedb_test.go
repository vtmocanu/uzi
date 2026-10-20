package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// claim_memory_authority_livedb_test.go pins issue #3142's ClaimRun predicate on the REAL
// query: with @memory_authority_lost a claimant gets only the kinds that never reach a memory
// runtime (judge, job, review task, profile-bound isolated run), while an ordinary issue run
// stays queued; with the flag false the issue run claims. One exempt fixture per OR arm of the
// predicate: judge, job, cross_check (run-lane child), review_target_run_id and
// egress_profile_id. The dedicated cross_check lane is exempt in Service.claimLane (it never
// sends the flag), proved below through ClaimCrossCheck. Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway Postgres (./e2e/run-store-it.sh).

func (f *isoFix) rawClaimParams(w store.Worker, lost bool) store.ClaimRunParams {
	now := time.Now()
	return store.ClaimRunParams{
		Lane:                  "ordinary",
		WorkerID:              pgconv.UUID(w.ID),
		UserID:                w.UserID,
		HeartbeatCutoff:       pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
		AffinityCutoff:        pgtype.Timestamptz{Time: now.Add(-25 * time.Minute), Valid: true},
		SpreadCutoff:          pgtype.Timestamptz{Time: now.Add(-9 * time.Second), Valid: true},
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
		WorkerCaps:            []string{},
		WorkerProtocolCaps:    w.ProtocolCapabilities,
		WorkerIsolatedLane:    w.IsolatedLane,
		IsEphemeral:           w.Ephemeral,
		EphemeralRunID:        w.EphemeralRunID,
		CodexCuratedModels:    codexCuratedModelsSlice(),
		MemoryAuthorityLost:   lost,
	}
}

// outcome claims through the query and returns the claimed run id (uuid.Nil when idle).
func (f *isoFix) outcome(t *testing.T, w store.Worker, lost bool) uuid.UUID {
	t.Helper()
	run, err := f.env.q.ClaimRun(f.env.ctx, f.rawClaimParams(w, lost))
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil
	}
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	return run.ID
}

func TestClaimMemoryAuthorityGateLiveDB(t *testing.T) {
	t.Run("issue run is withheld when lost, claimable otherwise", func(t *testing.T) {
		f := newIsoFix(t)
		run := f.queuedRun(t, 3142, false)
		w := f.seedWorker(t, laneWorkerSpec{protoCaps: capableCaps()})
		if got := f.outcome(t, w, true); got != uuid.Nil {
			t.Fatalf("lost claimant claimed %s, want idle", got)
		}
		f.refuse(t, w, run, "still queued after the gated claim") // Service.Claim: capable, member absent => lost
		if got := f.outcome(t, w, false); got != run {
			t.Fatalf("held claimant claimed %s, want %s", got, run)
		}
	})
	t.Run("task run is withheld when lost", func(t *testing.T) {
		f := newIsoFix(t)
		id := uuid.New()
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, dispatched_at, auto_approve, issue_title, issue_description, status)
		            VALUES ($1, $2, $3, 'task', $4, 'main', now(), true, 't', 'd', 'queued')`, id, f.userID, f.repo, "uzi/task/"+id.String())
		w := f.seedWorker(t, laneWorkerSpec{})
		if got := f.outcome(t, w, true); got != uuid.Nil {
			t.Fatalf("lost claimant claimed task %s, want idle", got)
		}
		if got := f.outcome(t, w, false); got != id {
			t.Fatalf("held claimant claimed %s, want %s", got, id)
		}
	})
	t.Run("judge run is exempt", func(t *testing.T) {
		f := newIsoFix(t)
		target := uuid.New()
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		            VALUES ($1, $2, $3, 'issue', 3143, 't', 'd', 'completed')`, target, f.userID, f.repo)
		id := uuid.New()
		f.env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, target_run_id)
		            VALUES ($1, $2, 'judge', 't', 'd', 'queued', $3)`, id, f.userID, target)
		w := f.seedWorker(t, laneWorkerSpec{})
		if got := f.outcome(t, w, true); got != id {
			t.Fatalf("lost claimant claimed %s, want the judge run %s", got, id)
		}
	})
	t.Run("job run is exempt", func(t *testing.T) {
		f := newIsoFix(t)
		id := uuid.New()
		f.env.exec(`INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, auto_approve, required_capabilities, status, budget_wall_seconds)
		            VALUES ($1, $2, 'job', 'research', 'job', 'prompt', true, '{}', 'queued', 600)`, id, f.userID)
		w := f.seedWorker(t, laneWorkerSpec{protoCaps: []string{capability.JobRunnerV1}})
		if got := f.outcome(t, w, true); got != id {
			t.Fatalf("lost claimant claimed %s, want the job run %s", got, id)
		}
	})
	t.Run("review task is exempt", func(t *testing.T) {
		f := newIsoFix(t)
		target := uuid.New()
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_requested, issue_title, issue_description, status, auto_approve)
		            VALUES ($1, $2, $3, 'task', $4, 'main', true, 't', 'd', 'completed', true)`, target, f.userID, f.repo, "uzi/task/"+target.String())
		id := uuid.New()
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_target_run_id, dispatched_at,
		                              auto_approve, open_mr, review_requested, issue_title, issue_description, status)
		            VALUES ($1, $2, $3, 'task', $4, 'main', $5, now(), true, false, false, 'review', '', 'queued')`,
			id, f.userID, f.repo, "uzi/task/"+target.String(), target)
		w := f.seedWorker(t, laneWorkerSpec{})
		if got := f.outcome(t, w, true); got != id {
			t.Fatalf("lost claimant claimed %s, want the review run %s", got, id)
		}
	})
	t.Run("isolated run is exempt for its lane worker", func(t *testing.T) {
		f := newIsoFix(t)
		id := f.queuedRun(t, 3144, true)
		w := f.laneWorker(t, id)
		if got := f.outcome(t, w, true); got != id {
			t.Fatalf("lost lane claimant claimed %s, want the isolated run %s", got, id)
		}
	})
}

// TestClaimMemoryAuthorityRegisterEvictsLiveDB: a gated claim records the worker, a fresh
// registration evicts the entry, and a held claim afterwards claims the run (issue #3142).
func TestClaimMemoryAuthorityRegisterEvictsLiveDB(t *testing.T) {
	f := newIsoFix(t)
	run := f.queuedRun(t, 3145, false)
	w := f.seedWorker(t, laneWorkerSpec{protoCaps: capableCaps()})
	f.refuse(t, w, run, "capable worker, member absent")
	if _, ok := f.svc.MemoryAuthorityLostFor(w.ID); !ok {
		t.Fatal("gated claim did not record the worker")
	}
	if _, _, err := f.svc.Register(f.env.ctx, store.Worker{ID: w.ID, UserID: f.userID}, "v1", "base", nil, nil, capableCaps(), nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := f.svc.MemoryAuthorityLostFor(w.ID); ok {
		t.Fatal("Register did not evict the entry")
	}
	reloaded, err := f.env.q.GetWorkerByID(f.env.ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	held := "held"
	p, err := f.svc.ClaimReported(f.env.ctx, reloaded, nil, MemoryAuthorityReport{Value: &held})
	if err != nil {
		t.Fatalf("held claim: %v", err)
	}
	if p == nil || p.RunID != run.String() {
		t.Fatalf("held claim = %+v, want run %s", p, run)
	}
}

// The cross_check OR arm: a legacy run-lane cross_check child is claimable by a lost claimant
// (it never reaches a memory runtime), through the raw query and through Service.Claim for a
// capable worker whose request omits the member.
func TestClaimMemoryAuthorityCrossCheckChildExemptLiveDB(t *testing.T) {
	t.Run("raw query, lost", func(t *testing.T) {
		fx := newClaudeCheckerFix(t, BindModePinned)
		w := wkrRow(t, fx.env, fx.workerID)
		p := crossCheckClaimParams(w)
		p.MemoryAuthorityLost = true
		run, err := fx.env.q.ClaimRun(fx.env.ctx, p)
		if err != nil || run.ID != fx.kid {
			t.Fatalf("lost claimant ClaimRun: id=%s err=%v, want the cross_check child %s", run.ID, err, fx.kid)
		}
	})
	t.Run("Service.Claim, capable and member absent", func(t *testing.T) {
		fx := newClaudeCheckerFix(t, BindModePinned)
		fx.env.exec(`UPDATE workers SET protocol_capabilities=array_cat(protocol_capabilities,$2) WHERE id=$1`, fx.workerID, capableCaps())
		w := wkrRow(t, fx.env, fx.workerID)
		p, err := fx.svc.Claim(fx.env.ctx, w, nil)
		if err != nil || p == nil || p.RunID != fx.kid.String() {
			t.Fatalf("Claim = %+v, %v; want the cross_check child %s", p, err, fx.kid)
		}
		if _, ok := fx.svc.MemoryAuthorityLostFor(w.ID); !ok {
			t.Error("the gated run-lane claim was not recorded")
		}
	})
}

// The dedicated cross_check lane is never gated and never recorded, even for a capable worker
// that omits the member.
func TestClaimMemoryAuthorityCrossCheckLaneNotGatedLiveDB(t *testing.T) {
	x := newCheckerCustodyFixture(t, "plan", "claude", "cross_check", false)
	x.env.exec(`UPDATE workers SET protocol_capabilities=array_cat(protocol_capabilities,$2) WHERE id=$1`, x.worker, capableCaps())
	w := wkrRow(t, x.env, x.worker)
	p, err := x.svc.ClaimCrossCheck(x.env.ctx, w, nil)
	if err != nil || p == nil || p.RunID != x.child.String() {
		t.Fatalf("ClaimCrossCheck = %+v, %v; want the checker child %s", p, err, x.child)
	}
	if _, ok := x.svc.MemoryAuthorityLostFor(w.ID); ok {
		t.Error("cross_check lane claim recorded a memory-authority entry")
	}
}
