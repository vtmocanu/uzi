package workersvc

import (
	"bytes"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func semanticOverrideReplayFixture(t *testing.T, selection bool) (*coverageLiveFixture, *apitypes.SemanticGateDTO, *AgentSelection) {
	t.Helper()
	f := newSemanticRevisionLive(t)
	f.structural.RequiredCapabilities = []string{"docker"}
	f.structural.RequiredTools = []string{"go"}
	f.structural.SizeClass = "m"
	f.submit(t)
	f.e.exec(t, `UPDATE runs SET repo_agents='[{"name":"reviewer"},{"name":"tester"}]' WHERE id=$1`, f.run)
	req := gateReportRequest(t, f)
	req.RequiredCapabilities, req.RequiredTools, req.SizeClass = &f.structural.RequiredCapabilities, &f.structural.RequiredTools, &f.structural.SizeClass
	gate := presentSemanticGate(t, f, req)
	var sel *AgentSelection
	if selection {
		sel = &AgentSelection{Source: AgentSourceRepo, Exclusions: []string{"reviewer"}}
	}
	return f, gate, sel
}

func TestSemanticGateOverrideReceiptReplayLiveDB(t *testing.T) {
	for _, selection := range []bool{false, true} {
		name := "nil_selection"
		if selection {
			name = "selection_and_exclusions"
		}
		t.Run(name, func(t *testing.T) {
			f, gate, sel := semanticOverrideReplayFixture(t, selection)
			opts := SubmitInputOptions{ExpectedGateRevision: &gate.Approval.PresentationRevision, SemanticApproval: &gate.Approval, OverrideCapabilities: true}
			first, err := f.svc.SubmitInputWithOptions(f.e.ctx, f.e.userID, f.run, "approve_plan", "original", sel, opts)
			if err != nil {
				t.Fatal(err)
			}
			run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
			if err != nil || len(run.RequiredCapabilities) != 0 || first.ID <= 0 || first.CreatedAt.IsZero() {
				t.Fatalf("override did not freeze and clear capabilities: %+v %v", first, err)
			}
			before := semanticRevisionSnapshot(t, f)
			retry := func(body string, selection *AgentSelection, options SubmitInputOptions) {
				t.Helper()
				result, err := f.svc.SubmitInputWithOptions(f.e.ctx, f.e.userID, f.run, "approve_plan", body, selection, options)
				if err != nil || result.ID != first.ID || !result.CreatedAt.Equal(first.CreatedAt) {
					t.Fatalf("original receipt lost: first=%+v replay=%+v err=%v", first, result, err)
				}
				if !bytes.Equal(before, semanticRevisionSnapshot(t, f)) {
					t.Fatal("replay changed persisted run/input/planning state")
				}
			}
			retry("original", sel, opts)
			opts.OverrideCapabilities = false
			retry("different body", &AgentSelection{Source: AgentSourceOwn}, opts)
			// A changed roster cannot turn an already frozen decision into a new
			// selection validation or update. The envelope is not decision identity.
			f.e.exec(t, "UPDATE runs SET repo_agents='[]' WHERE id=$1", f.run)
			before = semanticRevisionSnapshot(t, f)
			retry("different again", sel, opts)
			var count int
			if err := f.e.pool.QueryRow(f.e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1", f.run).Scan(&count); err != nil || count != 1 {
				t.Fatalf("approval input count=%d err=%v", count, err)
			}
		})
	}
}

func TestSemanticGateReceiptReplayAfterCapabilityChangeLiveDB(t *testing.T) {
	f, gate, sel := semanticOverrideReplayFixture(t, true)
	f.e.exec(t, "UPDATE workers SET capabilities=ARRAY['docker']::text[] WHERE id=$1", f.worker)
	first, err := semanticGateApprove(f, f.e.userID, gate, sel)
	if err != nil {
		t.Fatal(err)
	}
	f.e.exec(t, "UPDATE workers SET capabilities='{}' WHERE id=$1", f.worker)
	before := semanticRevisionSnapshot(t, f)
	replay, err := semanticGateApprove(f, f.e.userID, gate, sel)
	if err != nil || replay.ID != first.ID || !replay.CreatedAt.Equal(first.CreatedAt) || !bytes.Equal(before, semanticRevisionSnapshot(t, f)) {
		t.Fatalf("capability change invalidated original receipt: %+v %v", replay, err)
	}
}

func TestSemanticGateFirstFreezeStrictLiveDB(t *testing.T) {
	for _, scenario := range []string{"capabilities", "tools", "size", "milestones", "capability_gate", "roster"} {
		t.Run(scenario, func(t *testing.T) {
			f, gate, sel := semanticOverrideReplayFixture(t, true)
			opts := SubmitInputOptions{ExpectedGateRevision: &gate.Approval.PresentationRevision, SemanticApproval: &gate.Approval, OverrideCapabilities: true}
			switch scenario {
			case "capabilities":
				// Empty live requirements are only valid after an authorized freeze.
				f.e.exec(t, "UPDATE runs SET required_capabilities='{}' WHERE id=$1", f.run)
			case "tools":
				f.e.exec(t, "UPDATE runs SET required_tools='{}' WHERE id=$1", f.run)
			case "size":
				f.e.exec(t, "UPDATE runs SET size_class='l' WHERE id=$1", f.run)
			case "milestones":
				f.e.exec(t, `UPDATE runs SET milestones_candidate='[{"id":"m1","title":"different"}]' WHERE id=$1`, f.run)
			case "capability_gate":
				opts.OverrideCapabilities = false
			case "roster":
				f.e.exec(t, "UPDATE runs SET repo_agents='[]' WHERE id=$1", f.run)
			}
			before := semanticRevisionSnapshot(t, f)
			if _, err := f.svc.SubmitInputWithOptions(f.e.ctx, f.e.userID, f.run, "approve_plan", "", sel, opts); err == nil {
				t.Fatal("invalid first freeze accepted")
			}
			if !bytes.Equal(before, semanticRevisionSnapshot(t, f)) {
				t.Fatal("refused first freeze changed state")
			}
		})
	}
}

func TestSemanticGateFrozenReplayRefusalLiveDB(t *testing.T) {
	for _, scenario := range []string{"owner", "revision", "terminal", "released", "pending", "generation", "source_blob", "plan_blob", "receipt", "frozen_presentation", "frozen_approval", "current_candidate", "report", "maintainer"} {
		t.Run(scenario, func(t *testing.T) {
			f, gate, sel := semanticOverrideReplayFixture(t, false)
			opts := SubmitInputOptions{ExpectedGateRevision: &gate.Approval.PresentationRevision, SemanticApproval: &gate.Approval, OverrideCapabilities: true}
			if _, err := f.svc.SubmitInputWithOptions(f.e.ctx, f.e.userID, f.run, "approve_plan", "", sel, opts); err != nil {
				t.Fatal(err)
			}
			owner := f.e.userID
			switch scenario {
			case "owner":
				owner = f.worker
			case "revision":
				revision := gate.Approval.PresentationRevision + 1
				opts.ExpectedGateRevision = &revision
			case "terminal":
				f.e.exec(t, "UPDATE runs SET status='cancelled' WHERE id=$1", f.run)
			case "released":
				f.e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", f.run)
			case "pending":
				submitSemanticRevision(t, f, gate, "revise after approve", false)
			case "generation":
				f.e.exec(t, "UPDATE runs SET claim_generation=8 WHERE id=$1", f.run)
			case "source_blob":
				f.e.exec(t, "UPDATE semantic_snapshots SET raw=$3 WHERE run_id=$1 AND hash=$2", f.run, gate.Presentation.Candidate.Sources[0].Hash, []byte("corrupt"))
			case "plan_blob":
				f.e.exec(t, "DELETE FROM semantic_snapshots WHERE run_id=$1 AND hash=$2", f.run, gate.Presentation.Candidate.PlanHash)
			case "receipt":
				f.e.exec(t, "DELETE FROM run_user_inputs WHERE run_id=$1", f.run)
			case "frozen_presentation":
				f.e.exec(t, `UPDATE runs SET completion_contract=jsonb_set(completion_contract,'{semantic_presentation,report_digest}','"foreign"') WHERE id=$1`, f.run)
			case "frozen_approval":
				f.e.exec(t, `UPDATE runs SET completion_contract=jsonb_set(completion_contract,'{semantic_approval,report_digest}','"foreign"') WHERE id=$1`, f.run)
			case "current_candidate":
				f.e.exec(t, `UPDATE semantic_planning SET candidate=jsonb_set(candidate,'{digest}','"foreign"') WHERE run_id=$1`, f.run)
			case "report":
				opts.SemanticApproval.ReportDigest = "foreign"
			case "maintainer":
				opts.SemanticApproval.Maintainers = append(opts.SemanticApproval.Maintainers, apitypes.SemanticMaintainerApproval{ID: "foreign"})
			}
			before := semanticRevisionSnapshot(t, f)
			if _, err := f.svc.SubmitInputWithOptions(f.e.ctx, owner, f.run, "approve_plan", "retry", sel, opts); err == nil {
				t.Fatal("invalid frozen replay accepted")
			}
			if !bytes.Equal(before, semanticRevisionSnapshot(t, f)) {
				t.Fatal("refused replay changed persisted state")
			}
		})
	}
}
