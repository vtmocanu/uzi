package workersvc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func setManualCordonForTest(t *testing.T, env codexTestEnv, owner, worker uuid.UUID, cordon bool) store.Worker {
	t.Helper()
	w, err := store.ChangeWorkerManualCordon(env.ctx, env.pool, owner, worker, "lane operator",
		pgtype.Text{String: "investigating intake", Valid: true}, cordon)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// Both protocols must refuse unrelated intake without consuming or failing the
// checker; an existing affinity pin is the explicit continuation exception.
func TestManualCordonPlanCheckerLanesLiveDB(t *testing.T) {
	for _, lane := range []string{"run", "cross_check"} {
		for _, state := range []string{"unassigned", "assigned", "assigned-hard-fenced"} {
			t.Run(lane+"/"+state, func(t *testing.T) {
				f := newCrossCheckContentionFixture(t)
				f.env.exec("UPDATE workers SET last_heartbeat_at=now() WHERE id=$1", f.workerID)
				if lane == "cross_check" {
					f.env.exec("UPDATE workers SET max_cross_check_slots=1,protocol_capabilities=array_append(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1", f.workerID)
				}
				if state != "unassigned" {
					f.env.exec("UPDATE runs SET worker_id=$2 WHERE id=$1", f.runID, f.workerID)
				}
				setManualCordonForTest(t, f.env, f.userID, f.workerID, true)
				if state == "assigned-hard-fenced" {
					f.env.exec("UPDATE workers SET maintenance_fenced=true WHERE id=$1", f.workerID)
				}
				before := mustRun(t, f.env, f.runID)
				p := laneClaim(t, f, lane, nil)
				if state == "assigned" {
					f.claimed(t, p)
					return
				}
				if p != nil {
					t.Fatal("blocked checker delivered executable payload")
				}
				f.unchanged(t, before)
				setManualCordonForTest(t, f.env, f.userID, f.workerID, false)
				if state == "assigned-hard-fenced" {
					if laneClaim(t, f, lane, nil) != nil {
						t.Fatal("uncordon bypassed hard maintenance fence")
					}
					f.unchanged(t, before)
					f.env.exec("UPDATE workers SET maintenance_fenced=false WHERE id=$1", f.workerID)
				}
				f.claimed(t, laneClaim(t, f, lane, nil))
			})
		}
	}
}

func TestManualCordonCodeCheckerOwnAssignedLiveDB(t *testing.T) {
	f := codeFixture(t)
	cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), f.lead, 1,
		strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil || !cc.CheckerRunID.Valid {
		t.Fatalf("submit code checker: %v", err)
	}
	f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
	setManualCordonForTest(t, f.env, f.userID, f.workerID, true)
	before := mustRun(t, f.env, f.runID)
	f.env.exec("UPDATE workers SET maintenance_fenced=true WHERE id=$1", f.workerID)
	if laneClaim(t, f, "cross_check", nil) != nil {
		t.Fatal("hard-fenced assigned code checker delivered executable payload")
	}
	f.unchanged(t, before)
	f.env.exec("UPDATE workers SET maintenance_fenced=false WHERE id=$1", f.workerID)
	p := laneClaim(t, f, "cross_check", nil)
	r := mustRun(t, f.env, f.runID)
	if p == nil || p.RunID != f.runID.String() || p.CrossCheck == nil || p.CrossCheck.Stage != "code" ||
		r.Status != "claimed" || !r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != f.workerID || !r.CrossCheckLane {
		t.Fatal("manual cordon blocked own assigned code checker or lost persisted ownership")
	}
}

func TestManualCordonJudgeIntakeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	env.exec("UPDATE users SET judge_anthropic_bind_mode='default' WHERE id=$1", f.userID)
	target := f.seedRun("completed", 1)
	judge := uuid.New()
	env.exec(`INSERT INTO runs(id,user_id,kind,issue_title,issue_description,status,target_run_id)
		VALUES($1,$2,'judge','manual judge','review','queued',$3)`, judge, f.userID, target)
	f.svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})
	// Keep the pre-cordon API row: the locked DB read must remain authoritative.
	stale := wkrRow(t, env, f.workerID)
	setManualCordonForTest(t, env, f.userID, f.workerID, true)
	p, err := f.svc.Claim(env.ctx, stale, nil)
	if err != nil || p != nil {
		t.Fatalf("cordoned judge intake: payload=%v err=%v", p != nil, err)
	}
	r := mustRun(t, env, judge)
	if r.Status != "queued" || r.WorkerID.Valid || r.ClaimGeneration != 0 {
		t.Fatal("refused judge changed queued ownership")
	}
	setManualCordonForTest(t, env, f.userID, f.workerID, false)
	p, err = f.svc.Claim(env.ctx, stale, nil)
	r = mustRun(t, env, judge)
	if err != nil || p == nil || p.RunID != judge.String() || p.Kind != "judge" ||
		r.Status != "claimed" || !r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != f.workerID {
		t.Fatalf("uncordoned judge not delivered and persisted: err=%v", err)
	}
}

func TestManualCordonChatIntakeAndContinuationLiveDB(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		t.Run(fmt.Sprint("assigned=", assigned), func(t *testing.T) {
			env := setupCodexLiveDB(t)
			f := newCustodyContinuationFixture(t, env)
			id := uuid.New()
			var worker pgtype.UUID
			if assigned {
				worker = pgtype.UUID{Bytes: f.workerID, Valid: true}
			}
			env.exec(`INSERT INTO runs(id,user_id,kind,issue_title,issue_description,status,worker_id)
				VALUES($1,$2,'chat','manual chat','hello','queued',$3)`, id, f.userID, worker)
			stale := wkrRow(t, env, f.workerID)
			setManualCordonForTest(t, env, f.userID, f.workerID, true)
			if assigned {
				before := mustRun(t, env, id)
				env.exec("UPDATE workers SET maintenance_fenced=true WHERE id=$1", f.workerID)
				p, err := f.svc.ClaimChat(env.ctx, stale)
				after := mustRun(t, env, id)
				if err != nil || p != nil || after.Status != before.Status || after.WorkerID != before.WorkerID || after.ClaimGeneration != before.ClaimGeneration {
					t.Fatalf("hard-fenced assigned chat changed admission: payload=%v err=%v before=%+v after=%+v", p != nil, err, before, after)
				}
				env.exec("UPDATE workers SET maintenance_fenced=false WHERE id=$1", f.workerID)
			}
			p, err := f.svc.ClaimChat(env.ctx, stale)
			if err != nil {
				t.Fatal(err)
			}
			if !assigned {
				r := mustRun(t, env, id)
				if p != nil || r.Status != "queued" || r.WorkerID.Valid || r.ClaimGeneration != 0 {
					t.Fatal("cordoned chat accepted new intake")
				}
				setManualCordonForTest(t, env, f.userID, f.workerID, false)
				p, err = f.svc.ClaimChat(env.ctx, stale)
			}
			r := mustRun(t, env, id)
			if err != nil || p == nil || p.RunID != id.String() || r.Status != "claimed" ||
				!r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != f.workerID || r.ClaimGeneration != 1 {
				t.Fatalf("chat continuation/uncordon did not deliver persisted claim: err=%v", err)
			}
		})
	}
}

func TestManualCordonEndedChatContinueLiveDB(t *testing.T) {
	for _, cordoned := range []bool{false, true} {
		t.Run(fmt.Sprint("cordoned=", cordoned), func(t *testing.T) {
			env := setupCodexLiveDB(t)
			f := newCustodyContinuationFixture(t, env)
			src := uuid.New()
			env.exec(`INSERT INTO runs(id,user_id,kind,issue_title,issue_description,status,worker_id)
				VALUES($1,$2,'chat','ended chat','hello','completed',$3)`, src, f.userID, f.workerID)
			if cordoned {
				setManualCordonForTest(t, env, f.userID, f.workerID, true)
			}
			next, err := f.svc.ContinueChat(env.ctx, f.userID, src)
			if err != nil {
				t.Fatal(err)
			}
			if next.ID == src || next.Status != "queued" || !next.ResumeOfRunID.Valid || uuid.UUID(next.ResumeOfRunID.Bytes) != src ||
				next.WorkerID.Valid != !cordoned || next.ClaimGeneration != 0 {
				t.Fatalf("ended ContinueChat did not create correctly assigned new run: %+v", next)
			}
			if !cordoned && uuid.UUID(next.WorkerID.Bytes) != f.workerID {
				t.Fatal("uncordoned ContinueChat lost source-worker affinity")
			}
			stale := wkrRow(t, env, f.workerID)
			p, err := f.svc.ClaimChat(env.ctx, stale)
			if cordoned {
				r := mustRun(t, env, next.ID)
				if err != nil || p != nil || r.Status != "queued" || r.WorkerID.Valid || r.ClaimGeneration != 0 {
					t.Fatalf("ended chat broadened affinity under manual cordon: payload=%v err=%v run=%+v", p != nil, err, r)
				}
				setManualCordonForTest(t, env, f.userID, f.workerID, false)
				p, err = f.svc.ClaimChat(env.ctx, stale)
			}
			r := mustRun(t, env, next.ID)
			if err != nil || p == nil || p.RunID != next.ID.String() || r.Status != "claimed" ||
				!r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != f.workerID || r.ClaimGeneration != 1 {
				t.Fatalf("ended chat positive claim control failed: payload=%v err=%v run=%+v", p != nil, err, r)
			}
		})
	}
}

func TestManualCordonStaleWorkerCannotClaimOrdinaryLiveDB(t *testing.T) {
	f := newIsoFix(t)
	run := f.queuedRun(t, 3308, false)
	stale := wkrRow(t, f.env, f.workerID)
	setManualCordonForTest(t, f.env, f.userID, f.workerID, true)
	f.refuse(t, stale, run, "pre-cordon API worker row")
	setManualCordonForTest(t, f.env, f.userID, f.workerID, false)
	f.claimAs(t, stale, run)
}
