package workersvc

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func interactiveCodeFixture(t *testing.T) crossCheckContentionFixture {
	t.Helper()
	f := codeFixture(t)
	f.env.exec("UPDATE runs SET kind='task',issue_iid=NULL,branch='uzi/interactive-test',interactive=true,status='awaiting_followup' WHERE id=$1", f.lead)
	return f
}

func TestCodeCrossCheckInteractiveFinalizationLiveDB(t *testing.T) {
	f := interactiveCodeFixture(t)
	w := wkrRow(t, f.env, f.workerID)
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, identity := range []struct {
		worker     store.Worker
		generation int64
	}{{store.Worker{ID: uuid.New(), UserID: w.UserID}, 1}, {store.Worker{ID: w.ID, UserID: uuid.New()}, 1}, {w, 2}} {
		if _, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, identity.worker, f.lead, identity.generation, base, head); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("invalid submit custody accepted: %v", err)
		}
	}
	cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, base, head)
	if err != nil || !cc.CheckerRunID.Valid || cc.Outcome.String != "pending" {
		t.Fatalf("submit while awaiting follow-up: %+v %v", cc, err)
	}
	f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
	if p := laneClaim(t, f, "run", nil); p != nil {
		t.Fatal("code child escaped its lane")
	}
	foreignID := uuid.New()
	f.env.exec("INSERT INTO workers(id,user_id,name,token_hash,status,max_cross_check_slots,protocol_capabilities) SELECT $2,user_id,'foreign',$2::uuid::text::bytea,'online',1,protocol_capabilities FROM workers WHERE id=$1", w.ID, foreignID)
	foreign := wkrRow(t, f.env, foreignID)
	if p, err := f.svc.ClaimCrossCheck(f.env.ctx, foreign, nil); err != nil || p != nil {
		t.Fatalf("foreign claim: %+v %v", p, err)
	}
	p := laneClaim(t, f, "cross_check", nil)
	if p == nil || p.RunID != f.runID.String() || p.CrossCheck == nil || p.CrossCheck.Stage != "code" {
		t.Fatalf("same-worker code claim: %+v", p)
	}
	for _, identity := range []struct {
		worker     store.Worker
		generation int64
	}{{foreign, p.ClaimGeneration}, {w, p.ClaimGeneration + 1}, {store.Worker{ID: w.ID, UserID: uuid.New()}, p.ClaimGeneration}} {
		if _, err := f.svc.DecideCodeCrossCheck(f.env.ctx, identity.worker, f.runID, identity.generation, "completed", "", nil); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("invalid verdict identity accepted: %v", err)
		}
	}
	cc, err = f.svc.DecideCodeCrossCheck(f.env.ctx, w, f.runID, p.ClaimGeneration, "completed", "",
		[]CodeCrossCheckFinding{{ID: "F-1", Severity: "major", Title: "verify repair"}})
	if err != nil || cc.Outcome.String != "completed" {
		t.Fatalf("code verdict: %+v %v", cc, err)
	}
	// Custody evidence remains valid across lifecycle reports on the same claim.
	// These fixture transitions do not authorize a worker to wake the owner.
	// The durable evidence remains
	// bound to the same claim even if lifecycle reports move back to waiting.
	f.env.exec("UPDATE runs SET status='running' WHERE id=$1", f.lead)
	f.env.exec("UPDATE runs SET status='awaiting_followup' WHERE id=$1", f.lead)
	for _, identity := range []struct {
		worker     store.Worker
		generation int64
	}{{foreign, 1}, {w, 2}, {store.Worker{ID: w.ID, UserID: uuid.New()}, 1}} {
		if _, err := f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, identity.worker, f.lead, identity.generation, nil); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("invalid disposition identity accepted: %v", err)
		}
	}
	batch := []CodeCrossCheckDisposition{{ID: "F-1", Disposition: "addressed", Reason: "verified and fixed"}}
	cc, err = f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, w, f.lead, 1, batch)
	if err != nil || !cc.FinalizedAt.Valid || cc.InterruptedAt.Valid {
		t.Fatalf("waiting repair dispositions: %+v %v", cc, err)
	}
	retry, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, base, head)
	if err != nil || retry.ID != cc.ID {
		t.Fatalf("waiting submit retry: %+v %v", retry, err)
	}
	saved, err := f.svc.CodeCrossCheckStatus(f.env.ctx, w, f.lead, 1)
	if err != nil || saved.ID != cc.ID || saved.InterruptedAt.Valid {
		t.Fatalf("waiting status: %+v %v", saved, err)
	}
	if lead := mustRun(t, f.env, f.lead); lead.Status != "awaiting_followup" || lead.AutoApprove || lead.ClaimReleasedAt.Valid {
		t.Fatalf("advice changed lifecycle authority: %+v", lead)
	}
	rows, err := f.env.q.SetRunRunning(f.env.ctx, store.SetRunRunningParams{ID: f.lead, WorkerID: pgconv.UUID(w.ID)})
	if err != nil || rows != 0 {
		t.Fatalf("code advice opened owner wake gate: rows=%d err=%v", rows, err)
	}
	var inputs int
	if err := f.env.pool.QueryRow(f.env.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='follow_up'", f.lead).Scan(&inputs); err != nil || inputs != 0 {
		t.Fatalf("fabricated owner receipt: %d %v", inputs, err)
	}
	f.env.exec("UPDATE runs SET status='completed',claim_released_at=now() WHERE id=$1", f.lead)
	saved, err = f.env.q.GetCodeCrossCheck(f.env.ctx, f.lead)
	if err != nil || saved.InterruptedAt.Valid || !saved.FinalizedAt.Valid || saved.Outcome.String != "completed" {
		t.Fatalf("successful completion lost evidence: %+v %v", saved, err)
	}
}

func TestCodeCrossCheckInteractiveRefusalsLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, update string }{
		{"noninteractive", "interactive=false"},
		{"other-kind", "kind='issue',issue_iid=42"},
		{"other-wait", "status='awaiting_input'"},
		{"consent-off", "code_cross_check_required=false"},
		{"report-only", "report_only=true"},
		{"not-code", "fix_verdict='not_code'"},
		{"released", "claim_released_at=now()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := interactiveCodeFixture(t)
			f.env.exec("UPDATE runs SET "+tc.update+" WHERE id=$1", f.lead)
			w := wkrRow(t, f.env, f.workerID)
			if _, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40)); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("ineligible submission: %v", err)
			}
			if _, err := f.svc.RecordCodeCrossCheckFailure(f.env.ctx, w, f.lead, 1, "", "", "snapshot_failed"); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("ineligible failure evidence: %v", err)
			}
		})
	}
	t.Run("plan", func(t *testing.T) {
		f := laneFixture(t)
		f.env.exec("UPDATE runs SET kind='task',issue_iid=NULL,branch='uzi/interactive-test',interactive=true,status='awaiting_followup',worker_id=$2 WHERE id=$1", f.lead, f.workerID)
		if p := laneClaim(t, f, "cross_check", nil); p != nil {
			t.Fatal("waiting task admitted plan child")
		}
		if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), f.lead, 1, f.candidate); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("waiting plan submission: %v", err)
		}
	})
}

func TestCodeCrossCheckInteractiveCustodyLossLiveDB(t *testing.T) {
	for _, decided := range []bool{false, true} {
		state := "pending"
		if decided {
			state = "decided"
		}
		t.Run(state, func(t *testing.T) {
			for _, tc := range []struct{ name, update string }{
				{"generation", "claim_generation=2"},
				{"worker", "worker_id=NULL"},
				{"release", "claim_released_at=now()"},
				{"exit", "status='cancelled'"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f := codeFixture(t)
					f.env.exec("UPDATE runs SET kind='task',issue_iid=NULL,branch='uzi/interactive-test',interactive=true WHERE id=$1", f.lead)
					w := wkrRow(t, f.env, f.workerID)
					cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
					if err != nil {
						t.Fatal(err)
					}
					f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
					// Pending custody survives a finalizing lead's idle status.
					f.env.exec("UPDATE runs SET status='awaiting_followup' WHERE id=$1", f.lead)
					saved, err := f.svc.CodeCrossCheckStatus(f.env.ctx, w, f.lead, 1)
					if err != nil || saved.Outcome.String != "pending" || saved.InterruptedAt.Valid {
						t.Fatalf("park interrupted pending child: %+v %v", saved, err)
					}
					if decided {
						p := laneClaim(t, f, "cross_check", nil)
						if p == nil {
							t.Fatal("waiting checker not claimed")
						}
						if _, err := f.svc.DecideCodeCrossCheck(f.env.ctx, w, f.runID, p.ClaimGeneration, "completed", "",
							[]CodeCrossCheckFinding{{ID: "F-1", Severity: "major", Title: "verify"}}); err != nil {
							t.Fatal(err)
						}
					}
					f.env.exec("UPDATE runs SET "+tc.update+" WHERE id=$1", f.lead)
					saved, err = f.env.q.GetCodeCrossCheck(f.env.ctx, f.lead)
					if err != nil || !saved.InterruptedAt.Valid || saved.Outcome.String != "failed" {
						t.Fatalf("lost custody retained authority: %+v %v", saved, err)
					}
					if child := mustRun(t, f.env, f.runID); child.Status != "cancelled" || !child.ClaimReleasedAt.Valid {
						t.Fatalf("lost custody left child active: %+v", child)
					}
					if decided && !strings.Contains(string(saved.Findings), "F-1") {
						t.Fatal("interruption erased decided evidence")
					}
					if _, err := f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, w, f.lead, 1, nil); !errors.Is(err, ErrCrossCheckRefused) {
						t.Fatalf("interrupted repair accepted: %v", err)
					}
					child := mustRun(t, f.env, f.runID)
					if _, err := f.svc.DecideCodeCrossCheck(f.env.ctx, w, f.runID, child.ClaimGeneration, "completed", "", nil); !errors.Is(err, ErrCrossCheckRefused) {
						t.Fatalf("late verdict accepted after custody loss: %v", err)
					}
				})
			}
		})
	}
}

func TestCodeCrossCheckInteractiveFailureLiveDB(t *testing.T) {
	f := interactiveCodeFixture(t)
	w := wkrRow(t, f.env, f.workerID)
	for _, identity := range []struct {
		worker     store.Worker
		generation int64
	}{{store.Worker{ID: uuid.New(), UserID: w.UserID}, 1}, {store.Worker{ID: w.ID, UserID: uuid.New()}, 1}, {w, 2}} {
		if _, err := f.svc.RecordCodeCrossCheckFailure(f.env.ctx, identity.worker, f.lead, identity.generation, "", "", "snapshot_failed"); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("invalid failure custody accepted: %v", err)
		}
	}
	cc, err := f.svc.RecordCodeCrossCheckFailure(f.env.ctx, w, f.lead, 1, "", "", "snapshot_failed")
	if err != nil || cc.Outcome.String != "failed" || cc.CheckerRunID.Valid {
		t.Fatalf("waiting failure evidence: %+v %v", cc, err)
	}
	if lead := mustRun(t, f.env, f.lead); lead.Status != "awaiting_followup" {
		t.Fatal("failure woke waiting owner")
	}
}
