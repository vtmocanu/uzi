package workersvc

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestPolicyOwnerFreshAndPreserveLiveDB(t *testing.T) {
	for _, mode := range []string{"fresh", "preserve"} {
		t.Run(mode, func(t *testing.T) {
			e := setupPolicyHold(t)
			if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET session_id='prior-policy-session',policy_fresh_claim_generation=1 WHERE id=$1", e.runID); err != nil {
				t.Fatal(err)
			}
			held, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest())
			if err != nil {
				t.Fatal(err)
			}
			token := PolicyHoldActionToken(held)
			got, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, token, PolicyResumeRequest{Mode: mode, Guidance: "narrow scope"})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "fresh" {
				if got.SessionID.Valid || !got.PolicyFreshClaimGeneration.Valid || got.PolicyFreshClaimGeneration.Int64 != 2 {
					t.Fatalf("fresh identity: session=%v grant=%v", got.SessionID, got.PolicyFreshClaimGeneration)
				}
			} else if got.SessionID.String != "prior-policy-session" || got.PolicyFreshClaimGeneration.Valid {
				t.Fatalf("preserve identity: session=%v grant=%v", got.SessionID, got.PolicyFreshClaimGeneration)
			}
			var audit string
			if err := e.pool.QueryRow(e.ctx, "SELECT body FROM run_user_inputs WHERE run_id=$1 AND kind='resume'", e.runID).Scan(&audit); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(audit, mode) || (mode == "fresh" && !strings.Contains(audit, "prior-policy-session")) {
				t.Fatalf("audit=%q", audit)
			}
			if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET status='claimed',claim_generation=2 WHERE id=$1", e.runID); err != nil {
				t.Fatal(err)
			}
			rejected := PolicyHoldRequest{OutcomeID: "00000000-0000-4000-8000-000000000099", SourceGeneration: 2, ClaimGeneration: 2, Detail: "resume_rejected"}
			if _, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, rejected); err != nil {
				t.Fatal(err)
			}
			before := mustRun(t, e.codexTestEnv, e.runID)
			if _, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, token, PolicyResumeRequest{Mode: "fresh", Guidance: "duplicate"}); !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("lost response retry=%v", err)
			}
			after := mustRun(t, e.codexTestEnv, e.runID)
			if after.PolicyFreshClaimGeneration != before.PolicyFreshClaimGeneration || after.Status != "paused" {
				t.Fatal("stale retry changed grant/state")
			}
			var guidance string
			if err := e.pool.QueryRow(e.ctx, "SELECT string_agg(body,',') FROM run_user_inputs WHERE run_id=$1 AND kind='follow_up'", e.runID).Scan(&guidance); err != nil || guidance != "narrow scope" {
				t.Fatalf("guidance=%q err=%v", guidance, err)
			}
		})
	}
}

func TestPolicyOwnerSpentOrdinaryHoldLiveDB(t *testing.T) {
	for _, mode := range []string{"preserve", "fresh"} {
		t.Run(mode, func(t *testing.T) {
			e := setupPolicyHold(t)
			held, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET started_at=now()-interval '2 hours',status_since=now(),budget_wall_seconds=60 WHERE id=$1", e.runID); err != nil {
				t.Fatal(err)
			}
			before := mustRun(t, e.codexTestEnv, e.runID)
			got, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, PolicyHoldActionToken(held), PolicyResumeRequest{Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "queued" || got.StartedAt != before.StartedAt || got.BudgetExtensionSeconds != before.BudgetExtensionSeconds || got.BudgetWallSeconds != before.BudgetWallSeconds || got.BudgetFinalizeSeconds != before.BudgetFinalizeSeconds {
				t.Fatalf("resume changed budget or did not queue: %+v", got)
			}
		})
	}
}

func TestPolicyOwnerOverflowLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	_, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET claim_generation=$2 WHERE id=$1", e.runID, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	held := mustRun(t, e.codexTestEnv, e.runID)
	if _, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, PolicyHoldActionToken(held), PolicyResumeRequest{Mode: "fresh", Guidance: "must not write"}); err == nil {
		t.Fatal("overflow accepted")
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind IN ('resume','follow_up')", e.runID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("inputs=%d err=%v", n, err)
	}
}
