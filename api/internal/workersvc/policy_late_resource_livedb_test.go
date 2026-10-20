package workersvc

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPolicyLateHoldResourceProjectionLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	hold := latePolicyHold(t, e)
	latePolicyExec(t, e, "UPDATE recovery_custody_holds SET live_worker_id=$2,live_run_id=$3,created_at=now()-interval '10 years' WHERE id=$1", hold, e.wkr.ID, e.runID)
	latePolicyExec(t, e, "UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base',draining_since=now()-interval '1 hour',draining_reason='image_roll' WHERE id=$1", e.wkr.ID)
	_, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest())
	if err != nil || d != PolicyHoldApplied {
		t.Fatalf("hold=%s err=%v", d, err)
	}
	cutoff := pgconv.Time(time.Now().Add(-time.Minute))
	members, err := e.q.ListCustodyAdmissionMembersForOwner(e.ctx, store.ListCustodyAdmissionMembersForOwnerParams{UserID: e.userID, HeartbeatCutoff: cutoff})
	if err != nil || len(members) != 1 || !members[0].Counted || members[0].Reason != "run_not_live" {
		t.Fatalf("members=%+v err=%v", members, err)
	}
	limit, err := e.svc.custodyLimit(e.ctx)
	if err != nil {
		t.Fatalf("read configured custody limit: %v", err)
	}
	defaultLimit, err := strconv.ParseInt(settings.DefaultCustodyHoldLimit, 10, 32)
	if err != nil {
		t.Fatalf("parse default custody limit: %v", err)
	}
	if defaultLimit != 8 || int64(limit) != defaultLimit {
		t.Fatalf("configured custody limit=%d settings default=%d, want default 8", limit, defaultLimit)
	}
	aggregate, err := e.q.GetCustodyAggregateForOwner(e.ctx, store.GetCustodyAggregateForOwnerParams{UserID: e.userID, CustodyHoldLimit: limit, HeartbeatCutoff: cutoff})
	if err != nil || aggregate.AdmissionCountedHolds != 1 || aggregate.OpenHolds != 1 {
		t.Fatalf("aggregate=%+v err=%v", aggregate, err)
	}
	workers, err := e.q.ListHostedWorkersForController(e.ctx, store.ListHostedWorkersForControllerParams{HeartbeatCutoff: cutoff, DiskPressureMinStreak: 2})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range workers {
		if w.ID == e.wkr.ID {
			found = true
			if !w.Busy || !w.CustodyHeld || !w.DrainingSince.Valid {
				t.Fatalf("held worker not busy/draining: %+v", w)
			}
		}
	}
	if !found {
		t.Fatal("hosted worker absent")
	}
	// Old custody is still open and the policy pause has no age-based promotion.
	latePolicyExec(t, e, "UPDATE runs SET status_since=now()-interval '10 years' WHERE id=$1", e.runID)
	if r := mustRun(t, e.codexTestEnv, e.runID); r.Status != "paused" || r.ClaimReleasedAt.Valid {
		t.Fatal("hold lost unreleased pause")
	}
	latePolicyExec(t, e, "UPDATE workers SET ephemeral=true,ephemeral_run_id=$2 WHERE id=$1", e.wkr.ID, e.runID)
	idleWorker := uuid.New()
	latePolicyExec(t, e, "INSERT INTO workers(id,user_id,name,token_hash,status,ephemeral) VALUES($1,$2,'policy-unrelated-reap',$3,'offline',true)", idleWorker, e.userID, idleWorker[:])
	if _, err := store.ReapEphemeralWorkers(e.ctx, e.pool, pgconv.Time(time.Now().Add(time.Hour)), pgtype.Interval{}); err != nil {
		t.Fatal(err)
	}
	var heldExists, idleExists bool
	if err := e.pool.QueryRow(e.ctx, "SELECT EXISTS(SELECT 1 FROM workers WHERE id=$1), EXISTS(SELECT 1 FROM workers WHERE id=$2)", e.wkr.ID, idleWorker).Scan(&heldExists, &idleExists); err != nil {
		t.Fatal(err)
	}
	if !heldExists || idleExists {
		t.Fatalf("reaper must retain the held worker and remove the unrelated idle worker: held=%t idle=%t", heldExists, idleExists)
	}
	for generation := int32(2); generation <= limit; generation++ {
		latePolicyExec(t, e, `INSERT INTO recovery_custody_holds(user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
 VALUES($1,$2,$3,$4,'open',$5,'policy-resource')`, e.userID, e.repoID, e.runID, generation, e.wkr.ID)
	}
	fresh := uuid.New()
	latePolicyExec(t, e, `INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,claim_generation)
 VALUES($1,$2,$3,'issue',102,'fresh resource','d','queued',0)`, fresh, e.userID, e.repoID)
	admission, err := e.q.GetCustodyAdmissionForRun(e.ctx, store.GetCustodyAdmissionForRunParams{UserID: e.userID, RunID: fresh, CustodyHoldLimit: limit, HeartbeatCutoff: cutoff})
	if err != nil || admission.AdmissionCountedHolds != int64(limit) || admission.ContinuationExempt {
		t.Fatalf("default-eight fresh admission=%+v err=%v", admission, err)
	}
}
