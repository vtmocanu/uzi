package workersvc

import (
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
)

func TestPolicyHoldAuthorizationBeforeIdentityLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	req := policyRequest()
	held, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldApplied {
		t.Fatalf("hold=%s %v", d, err)
	}
	other := e.wkr
	other.ID = uuid.New()
	if _, err := e.pool.Exec(e.ctx, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'other',$3,'online')", other.ID, e.userID, other.ID[:]); err != nil {
		t.Fatal(err)
	}
	for _, newer := range []bool{false, true} {
		if newer {
			if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET policy_last_source_generation=2 WHERE id=$1", e.runID); err != nil {
				t.Fatal(err)
			}
			req.OutcomeID = uuid.NewString()
		}
		before := mustRun(t, e.codexTestEnv, e.runID)
		r, d, err := e.svc.ReportPolicyHold(e.ctx, other, e.runID, req)
		if !errors.Is(err, ErrRunNotOwned) || d != PolicyHoldPending || r.ID != uuid.Nil {
			t.Fatalf("wrong worker learned identity: %s %v %+v", d, err, r)
		}
		stale := req
		stale.ClaimGeneration = 2
		r, d, err = e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, stale)
		if !errors.Is(err, ErrRunNotOwned) || d != PolicyHoldPending || r.ID != uuid.Nil {
			t.Fatalf("wrong generation learned identity: %s %v", d, err)
		}
		if after := mustRun(t, e.codexTestEnv, e.runID); !reflect.DeepEqual(before, after) {
			t.Fatal("unauthorized report mutated run")
		}
	}
	_ = held
}

func TestPolicyHoldDuplicateCurrentGenerationLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	req := policyRequest()
	held, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldApplied {
		t.Fatalf("hold=%s %v", d, err)
	}
	if _, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, PolicyHoldActionToken(held), PolicyResumeRequest{Guidance: "once"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET status='claimed',claim_generation=2,policy_fresh_claim_generation=2 WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	before := mustRun(t, e.codexTestEnv, e.runID)
	req.ClaimGeneration = 2
	after, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldAlreadyApplied || !reflect.DeepEqual(before, after) {
		t.Fatalf("duplicate changed current owner action: %s %v", d, err)
	}
	// M1 has no grant producer; this injected future grant must survive deduplication.
	if after.PolicyFreshClaimGeneration.Int64 != 2 {
		t.Fatal("duplicate burned grant")
	}
	foreign := e.userID
	foreign[0] ^= 1
	if _, err := e.svc.ResumePolicyHold(e.ctx, foreign, e.runID, PolicyHoldActionToken(before), PolicyResumeRequest{}); !errors.Is(err, ErrRunNotOwned) {
		t.Fatalf("foreign resume=%v", err)
	}
}

func TestPolicyHoldCancelReturnsCurrentRowLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET stop_kind='cancelled' WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	r, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest())
	if err != nil || d != PolicyHoldTerminal || r.Status != "cancelled" || r.PolicyRefusalCount != 0 || r.WorkerID != pgconv.UUID(e.wkr.ID) {
		t.Fatalf("cancel response=%s status=%s count=%d err=%v", d, r.Status, r.PolicyRefusalCount, err)
	}
}
