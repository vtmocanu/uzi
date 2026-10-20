package workersvc

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Jobs remain subject to admission even though they also open no recovery hold.
func TestClaimJobCustodyControlLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, holds := range []int{7, 8} {
		t.Run(fmt.Sprintf("holds%d", holds), func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			caps := []string{"recovery_archive_v1", jobCap, jobFilesCap}
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, caps)
			job := uuid.New()
			env.exec(`INSERT INTO runs(id,user_id,kind,job_type,issue_title,issue_description,status,report_only)
				VALUES($1,$2,'job','research','job control','prompt','queued',true)`, job, f.userID)
			f.seedUnrelatedOpenHolds(holds)
			adm, err := env.q.GetCustodyAdmissionForRun(env.ctx, store.GetCustodyAdmissionForRunParams{
				UserID: f.userID, RunID: job, CustodyHoldLimit: testCustodyLimit,
				HeartbeatCutoff: pgconv.Time(time.Now().Add(-time.Minute)),
			})
			if err != nil || adm.AdmissionCountedHolds != int64(holds) || adm.ContinuationExempt {
				t.Fatalf("job admission precondition: %+v, %v", adm, err)
			}
			p, err := f.svc.Claim(env.ctx, wkrRow(t, env, f.workerID), nil)
			if err != nil {
				t.Fatal(err)
			}
			if holds == 7 {
				if p == nil || p.RunID != job.String() || p.Kind != "job" || p.Job == nil || p.ClaimGeneration != 1 {
					t.Fatal("otherwise eligible below-cap job was not delivered")
				}
			} else if p != nil {
				t.Fatal("at-cap report-only job bypassed custody admission")
			}
			r := mustRun(t, env, job)
			want := "queued"
			if holds == 7 {
				want = "claimed"
			}
			if r.Status != want {
				t.Fatalf("job status=%s want=%s", r.Status, want)
			}
			if got := f.openHolds(t); got != int64(holds) {
				t.Fatalf("job changed prior holds: %d want %d", got, holds)
			}
		})
	}
}

func TestClaimJudgeCustodyControlLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, holds := range []int{7, 8} {
		t.Run(fmt.Sprintf("holds%d", holds), func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			env.exec("UPDATE users SET judge_anthropic_bind_mode='default' WHERE id=$1", f.userID)
			target := f.seedRun("completed", 1)
			judge := uuid.New()
			env.exec(`INSERT INTO runs(id,user_id,kind,issue_title,issue_description,status,target_run_id)
    VALUES($1,$2,'judge','judge control','review','queued',$3)`, judge, f.userID, target)
			f.seedUnrelatedOpenHolds(holds)
			f.svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})
			p, err := f.svc.Claim(env.ctx, f.wkr, nil)
			if err != nil {
				t.Fatal(err)
			}
			if holds == 7 {
				if p == nil || p.RunID != judge.String() || p.Kind != "judge" || p.ClaimGeneration != 1 {
					t.Fatalf("eligible below-cap judge: %+v", p)
				}
			} else {
				if p != nil {
					t.Fatalf("at-cap judge delivered: %+v", p)
				}
				r := mustRun(t, env, judge)
				if r.Status != "queued" || r.ClaimGeneration != 0 {
					t.Fatalf("blocked judge changed: %+v", r)
				}
			}
			if got := f.openHolds(t); got != int64(holds) {
				t.Fatalf("judge changed holds: %d want %d", got, holds)
			}
		})
	}
}

func TestClaimCrossCheckCustodyAggregateControlLiveDB(t *testing.T) {
	f := newCheckerCustodyFixture(t, "plan", "codex", "cross_check", false)
	f.seed(t, 8)
	// A fresh publishing run contributes to blocked_runs but cannot displace
	// the checker: its worker affinity is pinned to the lead's other worker.
	lead := mustRun(t, f.env, f.lead)
	fresh := uuid.New()
	f.env.exec(`INSERT INTO runs(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status)
  VALUES($1,$2,$3,$4,'issue',9876,'publishing control','body','queued')`, fresh, f.owner, lead.RepoID, lead.WorkerID)
	aggregate := func() store.GetCustodyAggregateForOwnerRow {
		t.Helper()
		a, err := f.env.q.GetCustodyAggregateForOwner(f.env.ctx, store.GetCustodyAggregateForOwnerParams{
			UserID: f.owner, CustodyHoldLimit: testCustodyLimit, HeartbeatCutoff: pgconv.Time(time.Now().Add(-time.Minute)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if a.BlockedRuns != 1 || a.OpenHolds != 8 || a.AdmissionCountedHolds != 8 {
			t.Fatalf("aggregate must count only fresh publisher as blocked: %+v", a)
		}
		return a
	}
	before, evidence := aggregate(), f.snapshot(t)
	if p := f.claim(t); p == nil || p.RunID != f.child.String() {
		t.Fatalf("checker claim: %+v", p)
	}
	if r := mustRun(t, f.env, f.child); r.Status != "claimed" {
		t.Fatalf("child remains queued: %s", r.Status)
	}
	if r := mustRun(t, f.env, fresh); r.Status != "queued" || r.ClaimGeneration != 0 {
		t.Fatalf("publisher changed: %+v", r)
	}
	if after := aggregate(); after != before {
		t.Fatalf("aggregate changed: before=%+v after=%+v", before, after)
	}
	f.assertCount(t, 8)
	if after := f.snapshot(t); after != evidence {
		t.Fatal("checker changed prior custody holds/captures")
	}
}

func TestCrossCheckCustodyPlacementControlsLiveDB(t *testing.T) {
	for _, holds := range []int{8, 9} {
		t.Run(fmt.Sprintf("holds%d", holds), func(t *testing.T) {
			lane := laneFixture(t)
			f := checkerCustodyFixture{env: lane.env, svc: lane.svc, owner: lane.userID, worker: lane.workerID,
				lead: lane.lead, child: lane.runID, stage: "plan", harness: "codex", lane: "cross_check"}
			f.seed(t, holds)
			f.env.exec("UPDATE users SET ephemeral_workers_enabled=true WHERE id=$1", f.owner)
			f.env.exec("UPDATE workers SET status='offline' WHERE user_id=$1 AND id<>$2", f.owner, f.worker)
			f.env.exec("UPDATE runs SET worker_id=$2 WHERE id=$1", f.lead, f.worker)
			f.env.exec("UPDATE runs SET status_since=now()-interval '1 hour' WHERE id=$1", f.child)
			evidence := f.snapshot(t)
			check := func(wantCount int64, wantGap, wantSat bool) {
				t.Helper()
				now := time.Now()
				evaluated, cutoff := pgconv.Time(now), pgconv.Time(now.Add(-2*time.Minute))
				models := []string{customCodexModel}
				count, err := f.env.q.CountOnlineWorkersClaimableForRun(f.env.ctx, store.CountOnlineWorkersClaimableForRunParams{
					RunID: f.child, HeartbeatCutoff: pgconv.Time(now.Add(-45 * time.Second)), AffinityCutoff: cutoff,
					CrossCheckEvaluatedAt: evaluated, CrossCheckAffinityCutoff: cutoff, CapabilityAware: true,
					CodexCuratedModels: models, DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true},
				})
				if err != nil {
					t.Fatal(err)
				}
				if count.Claimable != wantCount {
					t.Fatalf("claimable=%d want=%d", count.Claimable, wantCount)
				}
				gaps, err := f.env.q.ListUnplaceableQueuedRunsForEphemeral(f.env.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
					CrossCheckEvaluatedAt: evaluated, CrossCheckAffinityCutoff: cutoff, BackgroundGraceCutoff: cutoff,
					CodexCuratedModels: models, DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true},
					MaxPerUser: 1000, MaxRows: 10000,
				})
				if err != nil {
					t.Fatal(err)
				}
				gap := false
				for _, r := range gaps {
					if r.ID == f.child {
						gap = true
					}
				}
				if gap != wantGap {
					t.Fatalf("gap=%v want=%v", gap, wantGap)
				}
				sats, err := f.env.q.ListSaturationQueuedRunsForEphemeral(f.env.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
					CrossCheckEvaluatedAt: evaluated, CrossCheckAffinityCutoff: cutoff, BackgroundGraceCutoff: cutoff,
					CodexCuratedModels: models, DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true},
					SaturationDelay: pgtype.Interval{Valid: true}, MaxPerUser: 1000, MaxRows: 10000,
				})
				if err != nil {
					t.Fatal(err)
				}
				sat := false
				for _, r := range sats {
					if r.ID == f.child {
						sat = true
					}
				}
				if sat != wantSat {
					t.Fatalf("saturation=%v want=%v", sat, wantSat)
				}
				f.assertCount(t, holds)
				if f.snapshot(t) != evidence {
					t.Fatal("placement readers changed custody evidence")
				}
			}
			t.Run("idle", func(t *testing.T) { check(1, false, false) })
			// Clone a valid child and claim it to occupy the sole dedicated slot.
			occupying := cloneLaneChild(t, lane)
			f.env.exec("UPDATE runs SET created_at=now()-interval '2 hours' WHERE id=$1", occupying)
			if p := f.claim(t); p == nil || p.RunID != occupying.String() {
				t.Fatalf("occupying claim: %+v", p)
			}
			t.Run("full", func(t *testing.T) { check(0, false, true) })
			f.env.exec("UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'cross_check_v1') WHERE id=$1", f.worker)
			t.Run("unavailable", func(t *testing.T) { check(0, true, false) })
		})
	}
}
