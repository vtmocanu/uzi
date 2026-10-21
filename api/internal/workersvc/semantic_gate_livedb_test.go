package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests deliberately live in workersvc, one of the seven wrapper roots.
// The lead owns execution through the serial LiveDB wrapper.
func newSemanticGateLive(t *testing.T) *coverageLiveFixture {
	t.Helper()
	f := newCoverageLiveFixture(t)
	f.svc.q = f.e.q
	f.svc.p = Params{RunMaxIterations: 5}
	f.svc.p.RunTimeout = 2 * 60 * 60 * 1000000000
	return f
}

func gateReportRequest(t *testing.T, f *coverageLiveFixture) StateRequest {
	t.Helper()
	var ms []Milestone
	if err := json.Unmarshal(f.structural.Milestones, &ms); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	return StateRequest{State: "awaiting_approval", ClaimGeneration: i64(7), PresentationID: &id, CandidateDigest: f.candidate.Digest, PlanMd: &f.structural.PlanMd, Milestones: &ms}
}
func presentSemanticGate(t *testing.T, f *coverageLiveFixture, req StateRequest) *apitypes.SemanticGateDTO {
	t.Helper()
	_, applied, revision, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, req)
	if err != nil || !applied || revision < 1 {
		t.Fatalf("gate report: applied=%v revision=%d err=%v", applied, revision, err)
	}
	run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.svc.SemanticGateForOwner(f.e.ctx, f.e.userID, run)
	if err != nil || out == nil {
		t.Fatalf("owner presentation: %v", err)
	}
	return out
}
func semanticGateApprove(f *coverageLiveFixture, owner uuid.UUID, gate *apitypes.SemanticGateDTO, sel *AgentSelection) (SubmitInputResult, error) {
	return f.svc.SubmitInputWithOptions(f.e.ctx, owner, f.run, "approve_plan", "", sel, SubmitInputOptions{ExpectedGateRevision: &gate.Approval.PresentationRevision, SemanticApproval: &gate.Approval})
}
func semanticGateState(t *testing.T, f *coverageLiveFixture) []byte {
	t.Helper()
	var raw []byte
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT jsonb_build_object('run',to_jsonb(r),'inputs',(SELECT jsonb_agg(i ORDER BY i.id) FROM run_user_inputs i WHERE i.run_id=r.id)) FROM runs r WHERE id=$1`, f.run).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
func publishGateCoverage(t *testing.T, f *coverageLiveFixture) []byte {
	t.Helper()
	r := f.reserve(t, "")
	used := 20
	f.settle(t, r, "success", &used)
	report := apitypes.SemanticCoverageReport{SemanticCoverageBinding: SemanticCoverageBinding(f.candidate), Requirements: []apitypes.SemanticCoverageResult{{RequirementID: "r1", Status: "represented", Explanation: "behavior represented", Citations: f.candidate.Criteria[0].Citations, CriterionIDs: []string{f.candidate.Criteria[0].ID}}}, Criteria: []apitypes.SemanticCoverageAtomicity{{CriterionID: f.candidate.Criteria[0].ID, Atomic: true, Explanation: "one behavior"}}}
	raw := semanticJSON(t, apitypes.SemanticCoveragePublishRequest{ReviewID: r.ReviewID, InvocationID: r.InvocationID, Report: report})
	if err := f.svc.PublishSemanticCoverage(f.e.ctx, f.worker, f.run, 7, raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSemanticGateApprovalReplayLiveDB(t *testing.T) {
	for _, selection := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_selection", true: "selection"}[selection], func(t *testing.T) {
			f := newSemanticGateLive(t)
			publication := publishGateCoverage(t, f)
			if err := f.svc.PublishSemanticCoverage(f.e.ctx, f.worker, f.run, 7, publication); err != nil {
				t.Fatal(err)
			}
			req := gateReportRequest(t, f)
			gate := presentSemanticGate(t, f, req)
			again := presentSemanticGate(t, f, req)
			if !sameSemanticJSON(gate, again) {
				t.Fatal("duplicate report changed presentation")
			}
			if gate.Presentation.Coverage.Outcome != "uncertain" || gate.Presentation.ReportDigest == "" || gate.Presentation.InvocationID == "" {
				t.Fatal("missing exact report identity")
			}
			if !bytes.Equal(gate.Sources[0].Bytes, f.sources[0].Bytes) {
				t.Fatal("immutable source bytes inaccessible")
			}
			var sel *AgentSelection
			if selection {
				sel = &AgentSelection{Source: AgentSourceOwn}
			}
			first, err := semanticGateApprove(f, f.e.userID, gate, sel)
			if err != nil {
				t.Fatal(err)
			}
			before := semanticGateState(t, f)
			second, err := semanticGateApprove(f, f.e.userID, gate, sel)
			if err != nil || first.ID != second.ID {
				t.Fatalf("lost response replay: %v %v %v", first, second, err)
			}
			if !bytes.Equal(before, semanticGateState(t, f)) {
				t.Fatal("replay mutated run/input")
			}
			run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
			if err != nil {
				t.Fatal(err)
			}
			contract, err := parseCompletionContract(run.CompletionContract)
			if err != nil {
				t.Fatal(err)
			}
			if contract.Profile != "semantic" || !sameSemanticJSON(contract.SemanticApproval, &gate.Approval) || !sameSemanticJSON(contract.SemanticPresentation, &gate.Presentation) || len(run.MilestonesFrozen) == 0 {
				t.Fatal("atomic freeze lost semantic contract")
			}
		})
	}
}

func TestSemanticGateRefusalWritesNothingLiveDB(t *testing.T) {
	for _, scenario := range []string{"foreign_owner", "unbound", "candidate", "criteria", "source", "generation", "cancel", "release", "maintainer", "report", "ordinary_answer", "no_tx"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSemanticGateLive(t)
			if scenario == "source" {
				f.sources[1].Completeness = "complete"
				f.sources[1].Bytes = []byte("original PRD requirement")
				f.submit(t)
			}
			req := gateReportRequest(t, f)
			gate := presentSemanticGate(t, f, req)
			originalBinding := SemanticCoverageBinding(f.candidate)
			owner := f.e.userID
			switch scenario {
			case "foreign_owner":
				owner = uuid.New()
			case "candidate", "criteria", "source":
				// Inject independent identity drift; revision admission has its own real-flow tests.
				f.e.exec(t, "UPDATE runs SET status='running' WHERE id=$1", f.run)
				switch scenario {
				case "candidate":
					f.structural.PlanMd += " changed"
				case "criteria":
					f.proposal.Criteria[0].DoneWhen += " independently verified"
				case "source":
					f.sources[1].Bytes = []byte("changed PRD requirement")
				}
				f.submit(t)
				if scenario == "source" && SemanticCoverageBinding(f.candidate).SourceDigest == originalBinding.SourceDigest {
					t.Fatal("source fixture did not change persisted source identity")
				}
				f.e.exec(t, "UPDATE runs SET status='awaiting_approval' WHERE id=$1", f.run)
			case "generation":
				f.e.exec(t, "UPDATE runs SET claim_generation=8 WHERE id=$1", f.run)
			case "cancel":
				f.e.exec(t, "UPDATE runs SET status='cancelled' WHERE id=$1", f.run)
			case "release":
				f.e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", f.run)
			case "maintainer":
				gate.Approval.Maintainers = append(gate.Approval.Maintainers, apitypes.SemanticMaintainerApproval{ID: uuid.NewString()})
			case "report":
				gate.Approval.ReportDigest = "foreign"
			case "no_tx":
				f.svc.txBeginner = nil
			}
			before := semanticGateState(t, f)
			var err error
			switch scenario {
			case "unbound":
				_, err = f.svc.SubmitInput(f.e.ctx, owner, f.run, "approve_plan", "", nil)
			case "ordinary_answer":
				_, err = f.svc.SubmitInput(f.e.ctx, owner, f.run, "answer", "[x] approve", nil)
			default:
				_, err = semanticGateApprove(f, owner, gate, nil)
			}
			if err == nil {
				t.Fatal("refusal accepted")
			}
			if !bytes.Equal(before, semanticGateState(t, f)) {
				t.Fatal("refused request wrote run or input")
			}
		})
	}
}

func TestSemanticGateMutationAndReportGenerationLiveDB(t *testing.T) {
	f := newSemanticRevisionLive(t)
	publication := publishGateCoverage(t, f)
	req := gateReportRequest(t, f)
	old := presentSemanticGate(t, f, req)
	revisionID := submitSemanticRevision(t, f, old, "clarify the criterion", true)
	f.proposal.Criteria[0].Text += " edited by human"
	f.submit(t)
	if err := f.svc.PublishSemanticCoverage(f.e.ctx, f.worker, f.run, 7, publication); err == nil {
		t.Fatal("stale report accepted")
	}
	newer := gateReportRequest(t, f)
	newer.ExpectedGateRevision = &old.Approval.PresentationRevision
	newer.RevisionInputID = &revisionID
	gate := presentSemanticGate(t, f, newer)
	if gate.Presentation.Coverage.Report != nil || gate.Presentation.Coverage.Outcome != "unavailable" || gate.Presentation.Coverage.ChargedTokens != 20 {
		t.Fatal("human revision reused certificate or reset budget")
	}
	if _, err := semanticGateApprove(f, f.e.userID, old, nil); err == nil {
		t.Fatal("historical presentation approved")
	}
	if _, err := semanticGateApprove(f, f.e.userID, gate, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSemanticGateConcurrentRevisionApprovalLiveDB(t *testing.T) {
	for _, approveFirst := range []bool{true, false} {
		name := "revise_before_approve"
		if approveFirst {
			name = "approve_before_revise"
		}
		t.Run(name, func(t *testing.T) {
			f := newSemanticRevisionLive(t)
			ctx, cancel := context.WithTimeout(f.e.ctx, 20*time.Second)
			defer cancel()
			f.e.ctx = ctx
			gate := presentSemanticGate(t, f, gateReportRequest(t, f))
			first, err := f.e.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Release()
			second, err := f.e.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Release()
			a := reconciliationRaceService(f.svc, store.New(first))
			b := reconciliationRaceService(f.svc, store.New(second))
			barrier := &reconciliationRaceBeginner{conn: first.Conn(), ready: make(chan struct{}), release: make(chan struct{})}
			a.SetTxBeginner(barrier)
			b.SetTxBeginner(second)
			var once sync.Once
			release := func() { once.Do(func() { close(barrier.release) }) }
			var wg sync.WaitGroup
			defer func() { release(); wg.Wait() }()
			approve := func(s *Service) error {
				_, err := s.SubmitInputWithOptions(ctx, f.e.userID, f.run, "approve_plan", "", nil, SubmitInputOptions{ExpectedGateRevision: &gate.Approval.PresentationRevision, SemanticApproval: &gate.Approval})
				return err
			}
			revise := func(s *Service) error {
				_, err := s.SubmitInputWithOptions(ctx, f.e.userID, f.run, "revise_plan", "more detail", nil, SubmitInputOptions{ExpectedGateRevision: &gate.Approval.PresentationRevision})
				return err
			}
			firstDone, secondDone := make(chan error, 1), make(chan error, 1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if approveFirst {
					firstDone <- approve(a)
					return
				}
				// Revision is normally autocommit. Enclose the real service
				// operation so its write holds the row lock at the commit barrier.
				var tx pgx.Tx
				tx, err := barrier.Begin(ctx)
				if err == nil {
					defer func() { _ = tx.Rollback(ctx) }()
					writer := reconciliationRaceService(f.svc, store.New(tx))
					err = revise(writer)
					if err == nil {
						err = tx.Commit(ctx)
					}
				}
				firstDone <- err
			}()
			reconciliationRaceWait(t, ctx, barrier.ready)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if approveFirst {
					secondDone <- revise(b)
				} else {
					secondDone <- approve(b)
				}
			}()
			reconciliationRaceBlocked(t, &gateFix{ctx: ctx, pool: f.e.pool}, second.Conn().PgConn().PID())
			release()
			wg.Wait()
			if err := <-firstDone; err != nil {
				t.Fatalf("first operation: %v", err)
			}
			secondErr := <-secondDone
			if approveFirst {
				if secondErr != nil {
					t.Fatalf("revision must succeed: %v", secondErr)
				}
			} else if !errors.Is(secondErr, ErrPlanRevisionPending) {
				t.Fatalf("approval = %v, want ErrPlanRevisionPending", secondErr)
			}
			var approvals, revisions, bound int
			if err := f.e.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE kind='approve_plan'),count(*) FILTER(WHERE kind='revise_plan'),count(*) FILTER(WHERE gate_binding='bound' AND gate_revision=$2) FROM run_user_inputs WHERE run_id=$1`, f.run, gate.Approval.PresentationRevision).Scan(&approvals, &revisions, &bound); err != nil {
				t.Fatal(err)
			}
			wantApprovals := 0
			if approveFirst {
				wantApprovals = 1
			}
			if approvals != wantApprovals || revisions != 1 || bound != approvals+revisions {
				t.Fatalf("approvals=%d revisions=%d bound=%d", approvals, revisions, bound)
			}
			run, err := f.e.q.GetRunByID(ctx, f.run)
			if err != nil {
				t.Fatal(err)
			}
			if run.GateRevision != gate.Approval.PresentationRevision || run.ReviseCount != 1 || run.Status != "awaiting_approval" || (len(run.CompletionContract) > 0) != approveFirst || (len(run.MilestonesFrozen) > 0) != approveFirst {
				t.Fatalf("unexpected revision/freeze state: revision=%d count=%d status=%s contract=%s frozen=%s", run.GateRevision, run.ReviseCount, run.Status, run.CompletionContract, run.MilestonesFrozen)
			}
			if approveFirst {
				contract, err := parseCompletionContract(run.CompletionContract)
				if err != nil || !sameSemanticJSON(contract.SemanticApproval, &gate.Approval) || !sameSemanticJSON(contract.SemanticPresentation, &gate.Presentation) {
					t.Fatalf("approval lost atomic semantic freeze: %v", err)
				}
			}
			before := semanticRevisionSnapshot(t, f)
			if _, err := semanticGateApprove(f, f.e.userID, gate, nil); !errors.Is(err, ErrPlanRevisionPending) {
				t.Fatalf("subsequent pending approval: %v", err)
			}
			if !bytes.Equal(before, semanticRevisionSnapshot(t, f)) {
				t.Fatal("pending approval wrote")
			}
		})
	}
}

func TestSemanticGateResetRefusesLiveDB(t *testing.T) {
	f := newSemanticGateLive(t)
	gate := presentSemanticGate(t, f, gateReportRequest(t, f))
	run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := ResetStateDigest(run)
	if err != nil {
		t.Fatal(err)
	}
	before := semanticGateState(t, f)
	_, err = f.svc.SubmitInputWithOptions(f.e.ctx, f.e.userID, f.run, "approve_plan", "", nil, SubmitInputOptions{
		ExpectedGateRevision: &gate.Approval.PresentationRevision, ExpectedResetStateDigest: digest,
		Reconciliation: &apitypes.MilestoneReconciliation{Mode: "reset", Reason: "replace contract"},
	})
	if !errors.Is(err, ErrSemanticResetUnsupported) {
		t.Fatalf("semantic reset: %v", err)
	}
	if !bytes.Equal(before, semanticGateState(t, f)) {
		t.Fatal("semantic reset mutated run or input")
	}
}

func freezeSemanticGateNoChecker(t *testing.T) *coverageLiveFixture {
	t.Helper()
	f := newSemanticGateLive(t)
	// Match a valid execution budget so changing size reaches the metadata
	// fence, rather than an unrelated zero-ceiling database constraint.
	f.svc.p.RunWallCeiling = 24 * time.Hour
	f.sources = f.sources[:1]
	f.structural.RequiredCapabilities = []string{"docker"}
	f.structural.RequiredTools = []string{"go", "node"}
	f.structural.SizeClass = "m"
	f.submit(t)
	publishGateCoverage(t, f)
	f.e.exec(t, "UPDATE runs SET auto_approve=true,plan_cross_check_required=false WHERE id=$1", f.run)
	req := gateReportRequest(t, f)
	req.State, req.PresentationID = "running", nil
	req.RequiredCapabilities, req.RequiredTools, req.SizeClass = &f.structural.RequiredCapabilities, &f.structural.RequiredTools, &f.structural.SizeClass
	_, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, req)
	if err != nil || !applied {
		t.Fatalf("freeze: applied=%v err=%v", applied, err)
	}
	run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := parseCompletionContract(run.CompletionContract)
	if err != nil || contract.Profile != "semantic" || run.PlanCrossCheckRequired ||
		!sameSemanticJSON(sortedSet(run.RequiredCapabilities), []string{"docker"}) ||
		!sameSemanticJSON(sortedSet(run.RequiredTools), []string{"go", "node"}) ||
		run.SizeClass != "m" || len(run.MilestonesFrozen) == 0 {
		t.Fatalf("fixture did not freeze semantic execution fields: %v", err)
	}
	return f
}

func TestSemanticGateFrozenNoCheckerReportsLiveDB(t *testing.T) {
	for _, scenario := range []string{"tools", "size", "caps", "plan", "milestones", "absent", "identical_metadata", "identical_plan", "filtered_noop", "progress"} {
		t.Run(scenario, func(t *testing.T) {
			f := freezeSemanticGateNoChecker(t)
			req := StateRequest{State: "running", ClaimGeneration: i64(7), IterationCount: 2}
			reject := true
			switch scenario {
			case "tools":
				tools := []string{"python"}
				req.RequiredTools = &tools
			case "size":
				size := "l"
				req.SizeClass = &size
			case "caps":
				caps := []string{"jvm"}
				req.RequiredCapabilities = &caps
			case "plan":
				req = gateReportRequest(t, f)
				req.State, req.PresentationID = "running", nil
				plan := f.structural.PlanMd + " changed"
				req.PlanMd = &plan
			case "milestones":
				req = gateReportRequest(t, f)
				req.State, req.PresentationID = "running", nil
				(*req.Milestones)[0].Title = "Changed definition"
			case "absent":
				reject = false
			case "identical_metadata", "identical_plan":
				reject = false
				if scenario == "identical_plan" {
					req = gateReportRequest(t, f)
					req.State, req.PresentationID = "running", nil
				}
				// Effective identity is set-based and uses the shared sanitiser.
				tools, caps, size := []string{"node", "go", "node", "unknown"}, []string{"docker", "docker"}, "m"
				req.RequiredTools, req.RequiredCapabilities, req.SizeClass = &tools, &caps, &size
			case "filtered_noop":
				reject = false
				tools, caps, size := []string{"unknown"}, []string{"unknown"}, "unknown"
				req.RequiredTools, req.RequiredCapabilities, req.SizeClass = &tools, &caps, &size
			case "progress":
				reject = false
				completed := []string{"m1"}
				req.MilestonesCompleted = &completed
			}
			before := semanticGateState(t, f)
			frozen, err := f.e.q.GetRunByID(f.e.ctx, f.run)
			if err != nil {
				t.Fatal(err)
			}
			_, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, req)
			if reject {
				if !errors.Is(err, ErrSemanticGateMismatch) || applied {
					t.Fatalf("changed %s: applied=%v err=%v", scenario, applied, err)
				}
				if !bytes.Equal(before, semanticGateState(t, f)) {
					t.Fatal("rejected report mutated run or inputs")
				}
				return
			}
			if err != nil || !applied {
				t.Fatalf("allowed %s: applied=%v err=%v", scenario, applied, err)
			}
			after, err := f.e.q.GetRunByID(f.e.ctx, f.run)
			if err != nil {
				t.Fatal(err)
			}
			if !sameSemanticJSON(sortedSet(after.RequiredCapabilities), sortedSet(frozen.RequiredCapabilities)) ||
				!sameSemanticJSON(sortedSet(after.RequiredTools), sortedSet(frozen.RequiredTools)) ||
				after.SizeClass != frozen.SizeClass || after.PlanMd != frozen.PlanMd ||
				!bytes.Equal(after.MilestonesFrozen, frozen.MilestonesFrozen) ||
				!bytes.Equal(after.CompletionContract, frozen.CompletionContract) {
				t.Fatal("allowed report changed frozen execution fields")
			}
			if scenario == "progress" && !sameSemanticJSON(json.RawMessage(after.MilestonesCompleted), json.RawMessage(`["m1"]`)) {
				t.Fatalf("progress not persisted: %s", after.MilestonesCompleted)
			}
		})
	}
}

func TestSemanticGateFrozenOwnerCapabilityOverrideLiveDB(t *testing.T) {
	f := newSemanticGateLive(t)
	f.structural.RequiredCapabilities = []string{"docker"}
	f.structural.RequiredTools = []string{"go"}
	f.structural.SizeClass = "m"
	f.submit(t)
	req := gateReportRequest(t, f)
	req.RequiredCapabilities, req.RequiredTools, req.SizeClass = &f.structural.RequiredCapabilities, &f.structural.RequiredTools, &f.structural.SizeClass
	gate := presentSemanticGate(t, f, req)
	_, err := f.svc.SubmitInputWithOptions(f.e.ctx, f.e.userID, f.run, "approve_plan", "", nil, SubmitInputOptions{
		ExpectedGateRevision: &gate.Approval.PresentationRevision, SemanticApproval: &gate.Approval, OverrideCapabilities: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	wkr := store.Worker{ID: f.worker, UserID: f.e.userID}
	if _, err := f.svc.ConsumeInputs(f.e.ctx, wkr, f.run, i64(7)); err != nil {
		t.Fatal(err)
	}
	run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil || len(run.RequiredCapabilities) != 0 || len(run.CompletionContract) == 0 || run.PlanCrossCheckRequired {
		t.Fatalf("owner override not frozen: %v", err)
	}
	snapshot, ok, err := decodeGatePresentedPayload(run.GatePresentedPayload)
	if err != nil || !ok || !sameSemanticJSON(snapshot.RequiredCapabilities, []string{"docker"}) {
		t.Fatalf("override fixture lost source display capabilities: %v", err)
	}
	// The immutable source display still names docker. The owner authorized an
	// empty execution requirement set, which absent/identical reports must keep.
	for _, scenario := range []string{"absent", "identical", "restore_source_caps"} {
		t.Run(scenario, func(t *testing.T) {
			req := StateRequest{State: "running", ClaimGeneration: i64(7)}
			if scenario == "identical" {
				caps := []string{}
				req.RequiredCapabilities, req.RequiredTools, req.SizeClass = &caps, &f.structural.RequiredTools, &f.structural.SizeClass
			}
			if scenario == "restore_source_caps" {
				req.RequiredCapabilities = &f.structural.RequiredCapabilities
			}
			before := semanticGateState(t, f)
			_, applied, _, err := f.svc.SetStateReport(f.e.ctx, wkr, f.run, req)
			if scenario == "restore_source_caps" {
				if !errors.Is(err, ErrSemanticGateMismatch) || applied || !bytes.Equal(before, semanticGateState(t, f)) {
					t.Fatalf("restored source capabilities: applied=%v err=%v", applied, err)
				}
				return
			}
			if err != nil || !applied {
				t.Fatalf("authorized %s: applied=%v err=%v", scenario, applied, err)
			}
			run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
			if err != nil || len(run.RequiredCapabilities) != 0 {
				t.Fatalf("override lost: %v", err)
			}
		})
	}
}

func TestSemanticGateNoCheckerAutoFenceLiveDB(t *testing.T) {
	for _, scenario := range []string{"complete", "missing_report", "maintainer", "delayed", "reclaimed", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSemanticGateLive(t)
			f.sources = f.sources[:1]
			if scenario == "maintainer" {
				f.proposal.Criteria[0].Kind = "maintainer"
				f.proposal.Criteria[0].Maintainer = &apitypes.SemanticMaintainer{Reason: "owner infrastructure", Conditions: []string{"available"}, VerificationSteps: []string{"observe"}}
			}
			f.submit(t)
			if scenario != "missing_report" {
				publishGateCoverage(t, f)
			}
			f.e.exec(t, "UPDATE runs SET auto_approve=true,plan_cross_check_required=false WHERE id=$1", f.run)
			req := gateReportRequest(t, f)
			req.State = "running"
			req.PresentationID = nil
			if scenario == "delayed" {
				req.CandidateDigest = "historical"
			}
			if scenario == "reclaimed" {
				f.e.exec(t, "UPDATE runs SET claim_generation=8 WHERE id=$1", f.run)
			}
			if scenario == "cancelled" {
				f.e.exec(t, "UPDATE runs SET status='cancelled' WHERE id=$1", f.run)
			}
			before := semanticGateState(t, f)
			_, applied, _, err := f.svc.SetStateReport(f.e.ctx, store.Worker{ID: f.worker, UserID: f.e.userID}, f.run, req)
			if scenario == "complete" {
				if err != nil || !applied {
					t.Fatalf("complete auto freeze: %v applied=%v", err, applied)
				}
				run, e := f.e.q.GetRunByID(f.e.ctx, f.run)
				if e != nil {
					t.Fatal(e)
				}
				contract, e := parseCompletionContract(run.CompletionContract)
				if e != nil || contract.Profile != "semantic" || contract.SemanticApproval != nil {
					t.Fatal("automatic freeze acquired human authority")
				}
			} else {
				if err == nil && applied {
					t.Fatal("unsafe automatic freeze")
				}
				if !bytes.Equal(before, semanticGateState(t, f)) {
					t.Fatal("automatic refusal wrote")
				}
			}
		})
	}
}
