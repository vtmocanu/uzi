package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Main executes these tests in the serial LiveDB sweep. No production enrollment
// writer is used: only this test injects a semantic contract into the existing row.
func TestSemanticCompletionOwnerAuthorizationReplayLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	id := e.seedFrozenRun(t, wid, []string{"m1"}, nil, false)
	fixture := semanticCompletionTwoMaintainers(t)
	e.exec(t, "UPDATE runs SET completion_contract=$2, milestones_frozen=$3 WHERE id=$1", id, fixture.CompletionContract, fixture.MilestonesFrozen)
	e.parkCompletionBlocked(t, id)
	e.seedPermit(t, id, wid, 1, "old-head")
	c, _ := parseCompletionContract(fixture.CompletionContract)
	dec := CompletionDecisionInput{Decision: "authorize_maintainer", Maintainers: semanticMaintainers(c)[:1], Reason: "authorize frozen owner steps", ContractRevision: 1}
	foreignAdmin := uuid.New()
	e.exec(t, "INSERT INTO users (id,email,password_hash,is_admin) VALUES ($1,$2,'hash',true)", foreignAdmin, "m4-"+foreignAdmin.String()+"@example.com")
	if _, err := svc.DecideCompletion(e.ctx, foreignAdmin, id, dec); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("foreign admin: %v", err)
	}
	run, err := svc.DecideCompletion(e.ctx, e.userID, id, dec)
	if err != nil || run.ContractRevision.Int32 != 2 {
		t.Fatalf("decision revision=%v err=%v", run.ContractRevision, err)
	}
	if !e.permitConsumed(t, id, 1, "old-head") {
		t.Fatal("old permit survived authorization revision")
	}
	if _, err := svc.DecideCompletion(e.ctx, e.userID, id, dec); err != nil {
		t.Fatal(err)
	}
	if n := e.countInputs(t, id, "completion_decision"); n != 1 {
		t.Fatalf("replay wrote %d decisions", n)
	}
	raw, rev := e.readContract(t, id)
	if rev == nil || *rev != 2 {
		t.Fatal("replay bumped revision again")
	}
	run.CompletionContract = raw
	pending, err := CompletionPending(run)
	if err != nil || len(pending.Criteria) != 2 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	for _, criterion := range pending.Criteria {
		want := 1
		if criterion.CriterionID == dec.Maintainers[0].ID {
			want = 2
		}
		if criterion.AuthorizationRevision != want || criterion.Status != "unverified" {
			t.Fatalf("selected authorization=%+v want revision %d", criterion, want)
		}
	}
	all := dec
	all.Maintainers = semanticMaintainers(c)
	if _, err := svc.DecideCompletion(e.ctx, e.userID, id, all); !errors.Is(err, ErrCompletionRevisionConflict) {
		t.Fatalf("all-authorizations falsely replayed selected decision: %v", err)
	}
	dec.Maintainers[0].Conditions = []string{"different"}
	if _, err := svc.DecideCompletion(e.ctx, e.userID, id, dec); !errors.Is(err, ErrCompletionRevisionConflict) {
		t.Fatalf("changed replay: %v", err)
	}
}

func TestSemanticCompletionPendingPermitLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	id := e.seedFrozenRun(t, wid, []string{"m1"}, nil, false)
	fixture := semanticCompletionFixture(t, false)
	e.exec(t, "UPDATE runs SET completion_contract=$2, milestones_frozen=$3 WHERE id=$1", id, fixture.CompletionContract, fixture.MilestonesFrozen)
	out, err := svc.RequestCompletionPermit(e.ctx, store.Worker{ID: wid, UserID: e.userID}, id, CompletionPermitRequest{ContractRevision: 1, Branch: "agent/issue-1", Head: "exact-head"})
	if err != nil || !out.Granted || out.Permit.CompletionPending == nil {
		t.Fatalf("permit=%+v err=%v", out, err)
	}
	run, err := e.q.GetRunByID(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := DecodeMilestoneIDs(run.MilestonesCompleted)
	if err != nil || len(completed) != 0 {
		t.Fatalf("pending marked milestone done: %v %v", completed, err)
	}
	// The completion transaction stamps the existing nonclosing scope-reduced
	// outcome; the pending criterion stays unverified in the same contract.
	_, applied, _, err := svc.SetStateReport(e.ctx, store.Worker{ID: wid, UserID: e.userID}, id, StateRequest{State: "completed", Branch: strPtr("agent/issue-1"), Head: strPtr("exact-head")})
	if err != nil || !applied {
		t.Fatalf("completion applied=%v err=%v", applied, err)
	}
	run, err = e.q.GetRunByID(e.ctx, id)
	if err != nil || run.StopKind.String != "scope_reduced" {
		t.Fatalf("nonclosing stop=%v err=%v", run.StopKind, err)
	}
	pending, err := CompletionPending(run)
	if err != nil || pending.Criteria[0].Status != "unverified" {
		t.Fatalf("completion verified pending: %+v %v", pending, err)
	}
}
