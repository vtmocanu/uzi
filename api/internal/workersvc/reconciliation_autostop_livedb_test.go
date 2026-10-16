package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReconciliationAutoStopOldCandidateLiveDB(t *testing.T) {
	for _, reclaim := range []bool{false, true} {
		t.Run(map[bool]string{false: "released", true: "same-worker-generation-two"}[reclaim], func(t *testing.T) {
			f, id, opts := newMilestoneReconciliationFix(t)
			now := time.Now()
			f.svc.now = func() time.Time { return now.Add(-autoStopWindow - time.Second) }
			f.exec("UPDATE runs SET status='running' WHERE id=$1", id)
			for i := 0; i < autoStopStreak; i++ {
				err := f.svc.AppendMessagesForClaim(f.ctx, f.wkr, id, []IncomingMessage{{Seq: 1, Kind: "tool_result", Payload: []byte(`{"n":1e1000000}`)}}, i64(1))
				if err == nil {
					t.Fatal("poison accepted")
				}
			}
			f.exec("UPDATE runs SET status='awaiting_approval' WHERE id=$1", id)
			var err error
			opts.ExpectedResetStateDigest, err = ResetStateDigest(reconciliationRow(t, f, id))
			if err != nil {
				t.Fatal(err)
			}
			f.svc.now = func() time.Time { return now }
			candidates := f.svc.persistFail.candidates(now)
			if len(candidates) != 1 {
				t.Fatalf("candidates: %+v", candidates)
			}
			f.svc.persistFail.recordSuccess(uuid.New(), now)
			if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
				t.Fatal(err)
			}
			if reclaim {
				claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
				if err != nil || claim == nil || claim.ClaimGeneration != 2 {
					t.Fatalf("claim: %+v %v", claim, err)
				}
				if _, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{State: "running", ClaimGeneration: i64(2)}); err != nil || !applied {
					t.Fatalf("running: %v %v", applied, err)
				}
			}
			before := reconciliationRow(t, f, id)
			if f.svc.evaluateAutoStop(f.ctx, now, candidates[0]) {
				t.Error("old candidate acted")
			}
			after := reconciliationRow(t, f, id)
			if after.Status != before.Status || after.StopKind.Valid {
				t.Errorf("old candidate changed successor: %s %v", after.Status, after.StopKind)
			}
			var late int
			if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM run_user_inputs i JOIN run_milestone_reconciliations r ON r.run_id=i.run_id WHERE i.run_id=$1 AND i.kind='cancel' AND i.id>r.stop_cutoff", id).Scan(&late); err != nil || late != 0 {
				t.Fatalf("late cancel past cutoff: %d %v", late, err)
			}
			if reclaim {
				f.svc.now = func() time.Time { return now.Add(-autoStopWindow - time.Second) }
				for i := 0; i < autoStopStreak; i++ {
					_ = f.svc.AppendMessagesForClaim(f.ctx, f.wkr, id, []IncomingMessage{{Seq: 1, Kind: "tool_result", Payload: []byte(`{"n":1e1000000}`)}}, i64(2))
				}
				f.svc.now = func() time.Time { return now }
				f.svc.p.AutoStopEnabled = true
				if got := f.svc.autoStopWedgedRuns(f.ctx, now); got != 1 {
					t.Fatalf("current generation auto-stop: %d", got)
				}
			}
		})
	}
}
