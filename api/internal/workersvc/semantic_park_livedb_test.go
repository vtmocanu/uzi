package workersvc

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1230 D3/A3: an autopilot semantic run whose server coverage projection is not complete, or
// which carries maintainer criteria, parks in awaiting_approval from running with a bounded
// semantic_park reason. Enrollment (semantic_planning) is seeded before any report.

// publishParkCoverage publishes one report on the fixture's current candidate. atomic=false makes
// the server projection incomplete.
func publishParkCoverage(t *testing.T, f *coverageLiveFixture, atomic bool) {
	t.Helper()
	r := f.reserve(t, "")
	used := 20
	f.settle(t, r, "success", &used)
	report := apitypes.SemanticCoverageReport{SemanticCoverageBinding: SemanticCoverageBinding(f.candidate),
		Requirements: []apitypes.SemanticCoverageResult{{RequirementID: "r1", Status: "represented", Explanation: "behavior represented", Citations: f.candidate.Criteria[0].Citations, CriterionIDs: []string{f.candidate.Criteria[0].ID}}},
		Criteria:     []apitypes.SemanticCoverageAtomicity{{CriterionID: f.candidate.Criteria[0].ID, Atomic: atomic, Explanation: "assessment"}}}
	raw := semanticJSON(t, apitypes.SemanticCoveragePublishRequest{ReviewID: r.ReviewID, InvocationID: r.InvocationID, Report: report})
	if err := f.svc.PublishSemanticCoverage(f.e.ctx, f.worker, f.run, 7, raw); err != nil {
		t.Fatal(err)
	}
}

// parkScenario prepares a fixture whose server projection yields the named coverage outcome (and
// maintainers) and returns the park reason the server requires ("" when none).
func parkScenario(t *testing.T, scenario string) (*coverageLiveFixture, string) {
	t.Helper()
	f := newSemanticGateLive(t)
	switch scenario {
	case "incomplete":
		f.sources = f.sources[:1]
		f.submit(t)
		publishParkCoverage(t, f, false)
		return f, semanticParkCoverageIncomplete
	case "uncertain":
		publishParkCoverage(t, f, true) // the fixture's second source has unknown completeness
		return f, semanticParkCoverageUncertain
	case "unavailable":
		f.sources = f.sources[:1]
		f.submit(t)
		return f, semanticParkCoverageUnavailable
	case "exhausted":
		f.sources = f.sources[:1]
		f.submit(t)
		for range 3 { // three unknown settlements spend the review and token budgets
			f.settle(t, f.reserve(t, ""), "unknown", nil)
		}
		return f, semanticParkCoverageUnavailable
	case "maintainer":
		f.sources = f.sources[:1]
		f.proposal.Criteria[0].Kind = "maintainer"
		f.proposal.Criteria[0].Maintainer = &apitypes.SemanticMaintainer{Reason: "owner infrastructure", Conditions: []string{"available"}, VerificationSteps: []string{"observe"}}
		f.submit(t)
		publishParkCoverage(t, f, true)
		return f, semanticParkMaintainerAuth
	case "complete":
		f.sources = f.sources[:1]
		f.submit(t)
		publishParkCoverage(t, f, true)
		return f, ""
	}
	t.Fatalf("unknown scenario %s", scenario)
	return nil, ""
}

func autopilotParkRequest(t *testing.T, f *coverageLiveFixture, park *string) StateRequest {
	t.Helper()
	req := gateReportRequest(t, f)
	req.SemanticPark = park
	return req
}

func storedPark(t *testing.T, run store.Run) (string, bool) {
	t.Helper()
	snapshot, ok, err := decodeGatePresentedPayload(run.GatePresentedPayload)
	if err != nil || !ok || snapshot.Semantic == nil {
		t.Fatalf("no stored semantic presentation: ok=%v err=%v", ok, err)
	}
	return snapshot.Semantic.SemanticPark, true
}

// Each non-complete projection, and a maintainer-bearing complete one, parks the autopilot run
// with the matching semantic_park stored only in the presented payload.
func TestSemanticCoverageParkLiveDB(t *testing.T) {
	for _, scenario := range []string{"incomplete", "uncertain", "unavailable", "exhausted", "maintainer"} {
		t.Run(scenario, func(t *testing.T) {
			f, want := parkScenario(t, scenario)
			f.e.exec(t, `UPDATE runs SET auto_approve=true,plan_cross_check_required=false,status_since=now()-interval '2 hours' WHERE id=$1`, f.run)
			lc := &fakeLifecycle{}
			f.svc.SetLifecycle(lc)
			req := autopilotParkRequest(t, f, strp(want))
			if _, applied, rev, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, req); err != nil || !applied || rev < 1 {
				t.Fatalf("park: applied=%v rev=%d err=%v", applied, rev, err)
			}
			run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != "awaiting_approval" || run.AutoApprove || len(run.CompletionContract) != 0 {
				t.Fatalf("park state: status=%s auto_approve=%v contract=%d bytes", run.Status, run.AutoApprove, len(run.CompletionContract))
			}
			if got, _ := storedPark(t, run); got != want {
				t.Fatalf("stored semantic_park=%q, want %q", got, want)
			}
			if run.PlanCrossCheckGateReason.Valid {
				t.Fatalf("semantic park leaked into plan_cross_check_gate_reason: %q", run.PlanCrossCheckGateReason.String)
			}
			var fresh bool
			if err := f.e.pool.QueryRow(f.e.ctx, `SELECT status_since > now() - interval '5 minutes' FROM runs WHERE id=$1`, f.run).Scan(&fresh); err != nil || !fresh {
				t.Fatalf("status_since not stamped at park: %v %v", fresh, err)
			}
			if len(lc.notes) != 1 || lc.notes[0].runID != f.run || lc.notes[0].status != "awaiting_approval" {
				t.Fatalf("gate notification hook = %+v, want one awaiting_approval for the run", lc.notes)
			}
			// The owner presentation exposes the reason.
			gate, err := f.svc.SemanticGateForOwner(f.e.ctx, f.e.userID, run)
			if err != nil || gate == nil || gate.Presentation.SemanticPark != want {
				t.Fatalf("owner presentation park=%v err=%v", gate, err)
			}
			// A lost-ACK replay of the same report is an exact replay, not a conflict.
			if _, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, req); err != nil || !applied {
				t.Fatalf("park replay: applied=%v err=%v", applied, err)
			}

			// Resume: the owner approves the displayed candidate; the wall time spent parked is banked.
			f.e.exec(t, `UPDATE runs SET status_since=now()-interval '1 hour' WHERE id=$1`, f.run)
			if _, err := semanticGateApprove(f, f.e.userID, gate, nil); err != nil {
				t.Fatalf("owner approval of the parked candidate: %v", err)
			}
			// The worker consumes the bound approval input before it reports running.
			f.e.exec(t, `UPDATE run_user_inputs SET applied_at=now() WHERE run_id=$1 AND kind='approve_plan'`, f.run)
			resume := StateRequest{State: "running", ClaimGeneration: i64(7)}
			if _, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, resume); err != nil || !applied {
				t.Fatalf("resume: applied=%v err=%v", applied, err)
			}
			resumed, err := f.e.q.GetRunByID(f.e.ctx, f.run)
			if err != nil {
				t.Fatal(err)
			}
			if resumed.Status != "running" || resumed.BudgetPausedSeconds < 3500 || resumed.BudgetPausedSeconds > 3700 {
				t.Fatalf("resume: status=%s budget_paused_seconds=%d, want running with ~3600 banked", resumed.Status, resumed.BudgetPausedSeconds)
			}
		})
	}
}

// With complete coverage and no maintainers, a required checker without APPROVE still parks the
// run, with no semantic_park and the existing plan_cross_check_gate_reason (a recorded non-pass
// verdict, or the deadline-to-human reason).
func TestSemanticCompleteCoverageCheckerGateKeepsExistingReasonLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, verdict, reason string
	}{
		{"recorded non-pass verdict", "block", "block"},
		{"deadline to human", "failed", "timed_out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := parkScenario(t, "complete")
			f.e.exec(t, `UPDATE runs SET auto_approve=true,plan_cross_check_required=true,harness='claude' WHERE id=$1`, f.run)
			child := uuid.New()
			f.e.exec(t, `INSERT INTO runs (id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description)
				VALUES ($1,$2,$3,'cross_check',$4,'codex',true,1800,'check','check')`, child, f.e.userID, f.e.repoID, f.run)
			f.e.exec(t, `INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,checker_run_id,checker_harness,verdict,reason_class,findings,decided_at,deadline_at)
				VALUES ($1,'plan',1,7,'plan','[]','{}','{}','s',repeat('a',40),'',$2,$3,'codex',$4,$5,'{"summary":"Human review required","items":[]}',now(),now()+interval '30 minutes')`,
				f.run, []byte("digest"), child, tc.verdict, tc.reason)
			req := autopilotParkRequest(t, f, nil)
			declared := tc.reason
			req.PlanCrossCheckGateReason = &declared
			if _, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, req); err != nil || !applied {
				t.Fatalf("park: applied=%v err=%v", applied, err)
			}
			run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != "awaiting_approval" || run.AutoApprove {
				t.Fatalf("park state: status=%s auto_approve=%v", run.Status, run.AutoApprove)
			}
			if got, _ := storedPark(t, run); got != "" {
				t.Fatalf("checker-only gate stored semantic_park=%q, want none", got)
			}
			if !run.PlanCrossCheckGateReason.Valid || run.PlanCrossCheckGateReason.String == "" {
				t.Fatalf("existing plan_cross_check_gate_reason lost: %+v", run.PlanCrossCheckGateReason)
			}
			if len(run.CompletionContract) != 0 {
				t.Fatal("checker-only park froze a contract")
			}
		})
	}
}

// The server compares the reported reason with its own projection on an auto_approve report and
// refuses every disagreement without writing anything.
func TestSemanticParkValidationLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scenario  string
		auto      bool
		supplied  *string
		wantStore bool // true: accepted
	}{
		{"incomplete, reason absent", "incomplete", true, nil, false},
		{"maintainer, reason absent", "maintainer", true, nil, false},
		{"uncertain, reason absent", "uncertain", true, nil, false},
		{"exhausted, reason absent", "exhausted", true, nil, false},
		{"incomplete, contradicting reason", "incomplete", true, strp(semanticParkCoverageUncertain), false},
		{"maintainer, coverage reason", "maintainer", true, strp(semanticParkCoverageIncomplete), false},
		{"exhausted, incomplete reason", "exhausted", true, strp(semanticParkCoverageIncomplete), false},
		{"exhausted, unavailable reason", "exhausted", true, strp(semanticParkCoverageUnavailable), true},
		{"unknown reason", "incomplete", true, strp("because"), false},
		{"empty reason", "incomplete", true, strp(""), false},
		{"complete without maintainers, reason supplied", "complete", true, strp(semanticParkCoverageIncomplete), false},
		{"complete without maintainers, maintainer reason supplied", "complete", true, strp(semanticParkMaintainerAuth), false},
		{"complete without maintainers, reason absent", "complete", true, nil, true},
		{"human-gated run, reason supplied", "incomplete", false, strp(semanticParkCoverageIncomplete), false},
		{"human-gated run, reason absent", "incomplete", false, nil, true},
		{"human-gated run, complete, reason supplied", "complete", false, strp(semanticParkCoverageIncomplete), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := parkScenario(t, tc.scenario)
			f.e.exec(t, `UPDATE runs SET auto_approve=$2,plan_cross_check_required=false WHERE id=$1`, f.run, tc.auto)
			before := semanticGateState(t, f)
			req := autopilotParkRequest(t, f, tc.supplied)
			_, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, req)
			if tc.wantStore {
				if err != nil || !applied {
					t.Fatalf("accepted report: applied=%v err=%v", applied, err)
				}
				run, gerr := f.e.q.GetRunByID(f.e.ctx, f.run)
				if gerr != nil {
					t.Fatal(gerr)
				}
				want := ""
				if tc.supplied != nil {
					want = *tc.supplied
				}
				if got, _ := storedPark(t, run); got != want {
					t.Fatalf("stored park=%q, want %q", got, want)
				}
				return
			}
			if !errors.Is(err, ErrSemanticGateMismatch) || applied {
				t.Fatalf("refused report: applied=%v err=%v, want ErrSemanticGateMismatch", applied, err)
			}
			if !bytes.Equal(before, semanticGateState(t, f)) {
				t.Fatal("refused park wrote")
			}
		})
	}
}

// A park supplied after the run already parked is refused unless it replays the stored reason.
func TestSemanticParkAfterParkedRefusalLiveDB(t *testing.T) {
	f, want := parkScenario(t, "incomplete")
	f.e.exec(t, `UPDATE runs SET auto_approve=true,plan_cross_check_required=false WHERE id=$1`, f.run)
	req := autopilotParkRequest(t, f, strp(want))
	if _, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, req); err != nil || !applied {
		t.Fatalf("park: applied=%v err=%v", applied, err)
	}
	// Re-presenting the parked gate without its reason keeps the stored, still-expected one.
	omitted := autopilotParkRequest(t, f, nil)
	if _, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, omitted); err != nil || !applied {
		t.Fatalf("omitted-reason re-presentation: applied=%v err=%v", applied, err)
	}
	if parked, err := f.e.q.GetRunByID(f.e.ctx, f.run); err != nil {
		t.Fatal(err)
	} else if got, _ := storedPark(t, parked); got != want {
		t.Fatalf("omitted reason erased the park: got %q want %q", got, want)
	}
	other := autopilotParkRequest(t, f, strp(semanticParkCoverageUncertain))
	if _, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, other); !errors.Is(err, ErrSemanticGateMismatch) || applied {
		t.Fatalf("contradicting replay: applied=%v err=%v", applied, err)
	}
	// A fresh presentation id does not launder a different reason onto the parked gate either.
	fresh := autopilotParkRequest(t, f, strp(semanticParkCoverageUncertain))
	if _, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, fresh); !errors.Is(err, ErrSemanticGateMismatch) || applied {
		t.Fatalf("fresh-id contradicting report: applied=%v err=%v", applied, err)
	}
}

// A checker APPROVE decided after the park neither unparks the lead nor freezes a contract.
func TestSemanticLateCheckerApproveDoesNotUnparkLiveDB(t *testing.T) {
	f, want := parkScenario(t, "incomplete")
	f.e.exec(t, `UPDATE runs SET auto_approve=true,plan_cross_check_required=true,harness='claude' WHERE id=$1`, f.run)
	checkerWorker := f.e.seedWorker(t, nil)
	child := uuid.New()
	f.e.exec(t, `INSERT INTO runs (id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description,status,worker_id,claim_generation)
		VALUES ($1,$2,$3,'cross_check',$4,'codex',true,1800,'check','check','running',$5,1)`, child, f.e.userID, f.e.repoID, f.run, checkerWorker)
	f.e.exec(t, `INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,checker_run_id,checker_harness,deadline_at)
		VALUES ($1,'plan',1,7,'plan','[]','{}','{}','s',repeat('a',40),'',$2,$3,'codex',now()+interval '30 minutes')`, f.run, []byte("digest"), child)
	req := autopilotParkRequest(t, f, strp(want))
	if _, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, req); err != nil || !applied {
		t.Fatalf("park: applied=%v err=%v", applied, err)
	}
	parked, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil || parked.Status != "awaiting_approval" || parked.AutoApprove {
		t.Fatalf("not parked: %+v %v", parked.Status, err)
	}
	before := semanticGateState(t, f)
	_, derr := f.svc.DecidePlanCrossCheck(f.e.ctx, store.Worker{ID: checkerWorker, UserID: f.e.userID}, child, 1, "approve", "", []byte(`{"summary":"fine","items":[]}`))
	t.Logf("late approve result: %v", derr)
	after, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "awaiting_approval" || after.AutoApprove || len(after.CompletionContract) != 0 || after.GateRevision != parked.GateRevision {
		t.Fatalf("late approve changed the parked lead: status=%s auto=%v contract=%d rev %d->%d", after.Status, after.AutoApprove, len(after.CompletionContract), parked.GateRevision, after.GateRevision)
	}
	// The server refuses a verdict for a lead that is no longer running, so nothing is written.
	if !errors.Is(derr, ErrCrossCheckRefused) || !bytes.Equal(before, semanticGateState(t, f)) {
		t.Fatalf("late approve was not a clean refusal: %v", derr)
	}
	if got, _ := storedPark(t, after); got != want {
		t.Fatalf("park changed to %q", got)
	}
	// The owner approval of the displayed gate is still required to leave the park.
	post, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil || post.Status != "awaiting_approval" {
		t.Fatalf("lead left the park: %s %v", post.Status, err)
	}
}

// A stale success from a reclaimed claim generation does not count as complete in either the
// worker status or the gate presentation, so a park is required (not an auto-freeze).
func TestSemanticStaleGenerationCoverageNotCompleteLiveDB(t *testing.T) {
	f, _ := parkScenario(t, "complete")
	if got := f.status(t).Outcome; got != "complete" {
		t.Fatalf("current-generation status=%q, want complete", got)
	}
	f.e.exec(t, `UPDATE runs SET claim_generation=8,auto_approve=true,plan_cross_check_required=false WHERE id=$1`, f.run)
	status, err := f.svc.SemanticCoverageStatus(f.e.ctx, f.worker, f.run, 8)
	if err != nil || status.Outcome == "complete" || status.Report != nil {
		t.Fatalf("stale success counted by the worker status: %+v %v", status, err)
	}
	run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := loadSemanticGate(f.e.ctx, f.e.q, run)
	if err != nil || p == nil || p.Coverage.Outcome == "complete" || p.Coverage.Report != nil {
		t.Fatalf("stale success counted by the gate projection: %+v %v", p, err)
	}
	if p.Coverage.Outcome != status.Outcome {
		t.Fatalf("callers disagree: projection=%q status=%q", p.Coverage.Outcome, status.Outcome)
	}
	// An automatic freeze on the new generation is refused rather than trusting the stale report.
	req := gateReportRequest(t, f)
	req.State, req.PresentationID, req.ClaimGeneration = "running", nil, i64(8)
	if _, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, req); err == nil && applied {
		t.Fatal("auto freeze accepted a stale-generation coverage success")
	}
}

// Auto-freeze on a required-checker semantic run needs the current checker APPROVE on this exact
// semantic candidate. That enforcement already exists in the autopilot freeze path
// (validateSemanticPlanWrite rehydrates the semantic checker from the immutable candidate history
// and compares digests; SetRunAutopilotPlan then binds the latest opposite-harness approval); this
// test pins that a missing approval, and an approve row that is not the semantic checker's own
// record of this candidate, are both refused without writing anything.
func TestSemanticAutoFreezeRequiresCheckerApproveLiveDB(t *testing.T) {
	for _, approved := range []bool{false, true} {
		name := map[bool]string{false: "no approval", true: "approve row without the semantic checker history"}[approved]
		t.Run(name, func(t *testing.T) {
			f := newSemanticGateLive(t)
			f.sources = f.sources[:1]
			f.structural.SizeClass = "s"
			f.submit(t)
			publishParkCoverage(t, f, true)
			f.e.exec(t, `UPDATE runs SET auto_approve=true,plan_cross_check_required=true,harness='claude',plan_source='agent' WHERE id=$1`, f.run)
			digest, err := hex.DecodeString(f.candidate.Digest)
			if err != nil {
				t.Fatal(err)
			}
			if approved {
				child := uuid.New()
				f.e.exec(t, `INSERT INTO runs (id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description)
					VALUES ($1,$2,$3,'cross_check',$4,'codex',true,1800,'check','check')`, child, f.e.userID, f.e.repoID, f.run)
				f.e.exec(t, `INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,checker_run_id,checker_harness,verdict,reason_class,findings,decided_at,deadline_at)
					VALUES ($1,'plan',1,7,$2,$3,'{}','{}','s',repeat('a',40),'',$4,$5,'codex','approve','approve','{"summary":"ok","items":[]}',now(),now()+interval '30 minutes')`,
					f.run, f.structural.PlanMd, []byte(f.structural.Milestones), digest, child)
			}
			req := gateReportRequest(t, f)
			caps, tools, size := []string{}, []string{}, "s"
			req.State, req.PresentationID = "running", nil
			req.RequiredCapabilities, req.RequiredTools, req.SizeClass = &caps, &tools, &size
			before := semanticGateState(t, f)
			_, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, req)
			if err == nil && applied {
				t.Fatal("auto freeze accepted without the semantic checker's APPROVE")
			}
			if !bytes.Equal(before, semanticGateState(t, f)) {
				t.Fatal("refused auto freeze wrote")
			}
		})
	}
}
