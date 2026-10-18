package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type reconciliationSessionReadFailure struct{ Store }

func (reconciliationSessionReadFailure) LatestRunMilestoneReconciliationSession(context.Context, uuid.UUID) ([][]byte, error) {
	return nil, errors.New("audit read unavailable")
}

func TestMilestoneReconciliationSessionReadFailsClosedLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	f.exec("UPDATE runs SET session_id='predecessor' WHERE id=$1", id)
	opts.ExpectedResetStateDigest, _ = ResetStateDigest(reconciliationRow(t, f, id))
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
		t.Fatal(err)
	}
	run := reconciliationRow(t, f, id)
	original := f.svc.q
	f.svc.q = reconciliationSessionReadFailure{original}
	if _, err := f.svc.assembleClaim(f.ctx, f.wkr, run); err == nil {
		t.Fatal("audit read error silently resumed session")
	}
	f.svc.q = original
	for _, raw := range []string{"{}", `{"session_id":17}`} {
		f.exec("UPDATE run_milestone_reconciliations SET before_state=$2::jsonb WHERE run_id=$1", id, raw)
		if _, err := f.svc.assembleClaim(f.ctx, f.wkr, run); err == nil {
			t.Fatalf("malformed snapshot %s resumed session", raw)
		}
	}
}

func TestMilestoneReconciliationHistoricalStopResetLiveDB(t *testing.T) {
	for _, kind := range []string{"scope_capped", "stopped", "cancelled", "plan_rejected", "scope_reduced"} {
		t.Run(kind, func(t *testing.T) {
			f, id, opts := newMilestoneReconciliationFix(t, kind)
			if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
				t.Fatalf("historical decision refused reset: %v", err)
			}
			assertReconciliationProgressEmpty(t, reconciliationRow(t, f, id))
		})
	}
}

func TestMilestoneReconciliationReviseInvalidatesWitnessLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	if _, err := f.svc.SubmitInput(f.ctx, f.userID, id, "revise_plan", "change the replacement", nil); err != nil {
		t.Fatal(err)
	}
	// Refresh consent only; retain the immutable witness invalidated by revise.
	opts.ExpectedResetStateDigest, _ = ResetStateDigest(reconciliationRow(t, f, id))
	approvalGuardRefused(t, f, id, nil, opts)
	// Settle the request to isolate stale-witness validation from the pending-revision
	// approval guard. Settlement alone cannot restore the invalidated witness.
	f.exec("UPDATE run_user_inputs SET applied_at=now() WHERE run_id=$1 AND kind='revise_plan'", id)
	// Even fresh consent cannot make the immutable old witness ready again.
	run := reconciliationRow(t, f, id)
	opts.ExpectedResetStateDigest, _ = ResetStateDigest(run)
	_, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts)
	if !errors.Is(err, ErrReconciliationNotReady) {
		t.Fatalf("revised source accepted: %v", err)
	}
	if reconciliationRow(t, f, id).ClaimReleasedAt.Valid {
		t.Fatal("revision released source")
	}
}
