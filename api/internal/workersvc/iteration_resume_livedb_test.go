package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestIterationSetTokenResumeLiveDB(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		name := "remaining_wall"
		if exhausted {
			name = "exhausted_wall"
		}
		t.Run(name, func(t *testing.T) {
			f := newCDFix(t)
			id := f.queuedRun(t, "issue")
			f.env.exec(`UPDATE runs SET status='paused',hold_reason='iteration_budget_exhausted',
 hold_captured_head='iteration-head',claim_generation=4,budget_max_iterations=5,iteration_count=99,
 status_since=now()-interval '10 minutes',claim_released_at=NULL WHERE id=$1`, id)
			if exhausted {
				f.env.exec("UPDATE runs SET started_at=now()-interval '3 hours' WHERE id=$1", id)
			}
			before := mustRun(t, f.env, id)
			// A credential becoming available is not an owner resume of an iteration hold.
			f.svc.RequestCredentialDisabledPromotion(f.userID)
			if after := mustRun(t, f.env, id); after.Status != "paused" || after.ClaimReleasedAt.Valid ||
				after.HoldReason != before.HoldReason || after.BudgetMaxIterations != before.BudgetMaxIterations {
				t.Fatalf("automatic credential promotion released iteration hold: %+v", after)
			}
			_, err := f.svc.SetRunCredential(f.env.ctx, f.userID, id, CredentialOverrideModePinned, &f.otherTok)
			after := mustRun(t, f.env, id)
			if exhausted {
				if !errors.Is(err, ErrCredentialSwitchRaced) {
					t.Fatalf("set-token without wall=%v", err)
				}
				// The existing set-token contract saves the owner's override before attempting
				// resume. Refusal must leave the held flight, head and wall accounting intact.
				if after.Status != "paused" || after.HoldReason != before.HoldReason ||
					after.HoldCapturedHead != before.HoldCapturedHead || after.ClaimReleasedAt.Valid ||
					after.BudgetPausedSeconds != before.BudgetPausedSeconds ||
					after.CodexClaimEpoch != before.CodexClaimEpoch {
					t.Fatalf("set-token refusal resumed/fenced hold: %+v", after)
				}
				return
			}
			if err != nil || after.Status != "queued" || after.HoldReason.Valid || after.HoldCapturedHead.Valid ||
				!after.ClaimReleasedAt.Valid || after.WorkerID != before.WorkerID || after.SessionID != before.SessionID ||
				after.ReleasedWorkerID.Valid || after.ReleasedWorkerNonce.Valid ||
				after.ClaimGeneration != 4 || after.BudgetMaxIterations != before.BudgetMaxIterations ||
				after.BudgetExtensionSeconds != before.BudgetExtensionSeconds || after.BudgetFinalizeSeconds != before.BudgetFinalizeSeconds ||
				after.BudgetPausedSeconds < 599 || after.StartedAt != before.StartedAt || after.IterationCount != 99 {
				t.Fatalf("explicit set-token resume=%+v err=%v", after, err)
			}
			// A new claim on the same worker continues the session with a fresh finite allowance.
			p, err := f.svc.Claim(f.env.ctx, f.worker(t), nil)
			if err != nil || p == nil || p.RunID != id.String() || p.ClaimGeneration != 5 ||
				p.SessionID == nil || *p.SessionID != "sess-cd" || p.Config.MaxIterations != 5 {
				t.Fatalf("continued service claim=%+v err=%v", p, err)
			}
			gen := p.ClaimGeneration
			if _, applied, err := f.svc.SetState(f.env.ctx, f.worker(t), id, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
				t.Fatalf("continued running applied=%v err=%v", applied, err)
			}
			r, applied, reason, err := f.svc.ReportIterationPark(f.env.ctx, f.worker(t), id, IterationParkEvidence{
				ClaimGeneration: gen, IterationCount: 5, MaxIterations: 5,
			})
			if err != nil || !applied || reason != "" || r.Status != "paused" || r.HoldReason.String != IterationBudgetExhausted {
				t.Fatalf("repeat invocation park applied=%v reason=%s err=%v run=%+v", applied, reason, err, r)
			}
		})
	}
}

func TestIterationHoldNoAutomaticReleaseLiveDB(t *testing.T) {
	f := newCDFix(t)
	id := f.queuedRun(t, "issue")
	f.env.exec(`UPDATE runs SET status='paused',hold_reason='iteration_budget_exhausted',
 claim_generation=4,budget_max_iterations=5,started_at=now()-interval '20 minutes',
 status_since=now()-interval '10 minutes',claim_released_at=NULL WHERE id=$1`, id)
	before := mustRun(t, f.env, id)
	// Sweep is the public periodic promotion/recovery seam; keep the worker fresh.
	f.env.exec("UPDATE workers SET last_heartbeat_at=now() WHERE id=$1", f.workerID)
	if _, err := f.svc.Sweep(f.env.ctx); err != nil {
		t.Fatal(err)
	}
	after := mustRun(t, f.env, id)
	if after.Status != "paused" || after.HoldReason != before.HoldReason || after.ClaimReleasedAt.Valid ||
		after.ClaimGeneration != 4 || after.BudgetMaxIterations != before.BudgetMaxIterations {
		t.Fatalf("periodic sweep released iteration hold: %+v", after)
	}
	// A sweeper caller scoped to weekly reserve cannot accidentally release an iteration hold.
	_, err := f.env.q.ResumePausedRun(f.env.ctx, store.ResumePausedRunParams{
		ID: id, UserID: f.userID, GlobalTimeoutSeconds: int32(time.Hour.Seconds()),
		RequiredHoldReason: pgconv.Text("weekly_reserve"),
	})
	if err == nil {
		t.Fatal("weekly reserve caller resumed iteration hold")
	}
}
