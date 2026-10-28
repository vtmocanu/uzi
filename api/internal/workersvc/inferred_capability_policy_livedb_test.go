package workersvc

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestIgnoredInferenceOrdinaryAndAutopilotLiveDB(t *testing.T) {
	for _, state := range []string{"awaiting_approval", "running"} {
		t.Run(state, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			workerID := e.seedWorker(t, nil)
			runID := e.seedActiveRunOwnedBy(t, workerID)
			e.exec(t, "UPDATE runs SET claim_generation=1,ignored_inferred_capabilities='{jvm}',auto_approve=$2 WHERE id=$1", runID, state == "running")
			svc := New(e.q, nil, testParams())
			svc.SetTxBeginner(e.pool)
			worker := store.Worker{ID: workerID, UserID: e.userID}
			caps, tools, size, plan := []string{"jvm"}, []string{"jvm"}, "s", "Update docs"
			id := uuid.New()
			req := StateRequest{State: state, ClaimGeneration: i64(1), PlanMd: &plan, RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size}
			if state == "awaiting_approval" {
				req.PresentationID = &id
			}
			_, applied, revision, err := svc.SetStateReport(e.ctx, worker, runID, req)
			if err != nil || !applied {
				t.Fatalf("state=%s applied=%v err=%v", state, applied, err)
			}
			row, err := e.q.GetRunByID(e.ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if len(row.RequiredCapabilities) != 0 || !reflect.DeepEqual(row.RequiredTools, tools) {
				t.Fatalf("caps=%v tools=%v", row.RequiredCapabilities, row.RequiredTools)
			}
			_, applied, replayRevision, err := svc.SetStateReport(e.ctx, worker, runID, req)
			if err != nil || !applied || replayRevision != revision {
				t.Fatalf("replay applied=%v revision=%d want=%d err=%v", applied, replayRevision, revision, err)
			}
			if state == "awaiting_approval" {
				if _, err := svc.SubmitInput(e.ctx, e.userID, runID, "approve_plan", "", nil); err != nil {
					t.Fatalf("base worker approval: %v", err)
				}
			}
		})
	}
}

func TestIgnoredInferenceSemanticGateReplayLiveDB(t *testing.T) {
	for _, hint := range []bool{false, true} {
		t.Run(map[bool]string{false: "suppressed", true: "hint wins"}[hint], func(t *testing.T) {
			f := newSemanticGateLive(t)
			want := []string{}
			if hint {
				want = []string{"jvm"}
			}
			f.e.exec(t, "UPDATE runs SET ignored_inferred_capabilities='{jvm}',required_capabilities=$2 WHERE id=$1", f.run, want)
			f.structural.RequiredCapabilities, f.structural.RequiredTools = []string{"jvm"}, []string{"jvm"}
			f.submit(t)
			digest := f.candidate.Digest
			oversized := f.structural
			oversized.RequiredCapabilities = make([]string, 65)
			for i := range oversized.RequiredCapabilities {
				oversized.RequiredCapabilities[i] = "jvm"
			}
			if _, err := f.svc.SubmitSemanticCandidate(f.e.ctx, f.worker, f.run, 7, oversized, semanticJSON(t, f.proposal), f.sources); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("suppression must not bypass the raw candidate bound: %v", err)
			}
			fields, err := semanticPlanFieldsDigest(f.structural.PlanMd, f.structural.Milestones, want, f.structural.RequiredTools, f.structural.SizeClass)
			if err != nil || f.candidate.PlanFieldsDigest != fields {
				t.Fatalf("effective semantic identity: %v", err)
			}
			publishGateCoverage(t, f)
			req := gateReportRequest(t, f)
			req.RequiredCapabilities, req.RequiredTools, req.SizeClass = &f.structural.RequiredCapabilities, &f.structural.RequiredTools, &f.structural.SizeClass
			gate := presentSemanticGate(t, f, req)
			f.submit(t)
			if f.candidate.Digest != digest {
				t.Fatal("same inference changed semantic identity on replay")
			}
			presentSemanticGate(t, f, req)
			_, err = semanticGateApprove(f, f.e.userID, gate, nil)
			if hint && !errors.Is(err, ErrCapabilityUnmet) {
				t.Fatalf("declared hint must still block: %v", err)
			}
			if !hint && err != nil {
				t.Fatalf("suppressed inference must approve: %v", err)
			}
		})
	}
}

func TestIgnoredInferenceCrossCheckReplayLiveDB(t *testing.T) {
	env, svc, worker, runID, candidate := newAutomaticRoundsFixture(t, 0)
	env.exec("UPDATE runs SET ignored_inferred_capabilities='{jvm}' WHERE id=$1", runID)
	candidate.RequiredCapabilities, candidate.RequiredTools = []string{"jvm"}, []string{"jvm"}
	checked, err := svc.SubmitPlanCrossCheck(env.ctx, worker, runID, 1, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if len(checked.RequiredCapabilities) != 0 || !reflect.DeepEqual(checked.RequiredTools, candidate.RequiredTools) {
		t.Fatalf("wrong candidate requirements: %+v", checked)
	}
	// Changing the live repo setting cannot alter this run's captured policy.
	env.exec("UPDATE repos SET ignored_inferred_capabilities='{}' WHERE id=(SELECT repo_id FROM runs WHERE id=$1)", runID)
	replay, err := svc.SubmitPlanCrossCheck(env.ctx, worker, runID, 1, candidate)
	if err != nil || replay.ID != checked.ID || !bytes.Equal(replay.CandidateDigest, checked.CandidateDigest) {
		t.Fatalf("cross-check replay: %v", err)
	}
	env.exec("UPDATE cross_checks SET verdict='approve',reason_class='approve' WHERE id=$1", checked.ID)
	ms := []Milestone{}
	req := StateRequest{State: "running", ClaimGeneration: i64(1), PlanMd: &candidate.PlanMd, Milestones: &ms, RequiredCapabilities: &candidate.RequiredCapabilities, RequiredTools: &candidate.RequiredTools, SizeClass: &candidate.SizeClass, CandidateDigest: hex.EncodeToString(checked.CandidateDigest)}
	_, applied, err := svc.SetState(env.ctx, worker, runID, req)
	if err != nil || !applied {
		t.Fatalf("cross-check canonical write applied=%v err=%v", applied, err)
	}
	row := mustRun(t, env, runID)
	if len(row.RequiredCapabilities) != 0 || !reflect.DeepEqual(row.RequiredTools, candidate.RequiredTools) {
		t.Fatalf("wrong persisted requirements: %v %v", row.RequiredCapabilities, row.RequiredTools)
	}
}
