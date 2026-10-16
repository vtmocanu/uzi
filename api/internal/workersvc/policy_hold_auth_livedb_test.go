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
		if err != nil || d != PolicyHoldUnauthenticatedHistoricalOutcome || r.ID != e.runID {
			t.Fatalf("missing historical authority bypassed refusal: %s %v", d, err)
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
	// A grant for the current claim must survive delivery of an already applied outcome.
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

func TestPolicyHoldBlankSessionPreservesIdentityLiveDB(t *testing.T) {
	for _, blank := range []string{"", " \t"} {
		t.Run("blank_"+blank, func(t *testing.T) {
			e := setupPolicyHold(t)
			before := mustRun(t, e.codexTestEnv, e.runID)
			req := policyRequest()
			req.SessionID = &blank
			_, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
			if !errors.Is(err, ErrInvalidState) {
				t.Fatalf("blank conversation identifier accepted: %v", err)
			}
			if after := mustRun(t, e.codexTestEnv, e.runID); !reflect.DeepEqual(before, after) {
				t.Fatal("blank session report altered conversation or hold state")
			}
		})
	}
}

func TestPolicyHoldRevisionWaitBankedLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET status='awaiting_approval',plan_md='Original plan',gate_revision=7,started_at=now()-interval '3 hours',status_since=now()-interval '2 hours',budget_paused_seconds=11 WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, []IncomingMessage{{Seq: 1, Kind: "plan_revising", Payload: []byte("{}")}}, i64p(1)); err != nil {
		t.Fatal(err)
	}
	before := mustRun(t, e.codexTestEnv, e.runID)
	req := policyRequest()
	req.ExpectedGateRevision = i64p(7)
	held, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldApplied {
		t.Fatalf("hold: %s %v", d, err)
	}
	if delta := held.BudgetPausedSeconds - before.BudgetPausedSeconds; delta < 7200 || delta > 7260 {
		t.Fatalf("lost the preceding two-hour gate wait: banked=%d seconds", delta)
	}
	duplicate, d, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
	if err != nil || d != PolicyHoldAlreadyApplied || duplicate.BudgetPausedSeconds != held.BudgetPausedSeconds {
		t.Fatalf("duplicate banked gate wait twice: %s %v", d, err)
	}
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET status_since=now()-interval '1 minute' WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	resumed, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, PolicyHoldActionToken(held), PolicyResumeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if delta := resumed.BudgetPausedSeconds - held.BudgetPausedSeconds; delta < 60 || delta > 120 {
		t.Fatalf("policy hold wait not banked separately: %d seconds", delta)
	}
	if !resumed.StartedAt.Time.Equal(before.StartedAt.Time) {
		t.Fatal("resume reset the execution baseline")
	}
}
