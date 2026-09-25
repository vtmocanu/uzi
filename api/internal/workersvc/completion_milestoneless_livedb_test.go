package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1626 LiveDB coverage: an INTERLOCKED run whose plan has NO milestones must still freeze
// a completion contract (criteria:[] at revision 1) so the permit grants vacuously, instead of
// holding at finalize with "completion identity unresolvable". The worker omits an empty
// milestone list, so the server reads a milestone-less PLAN-BEARING report as the explicit `[]`
// (planMilestonesParam). These drive the real Service (SetState / SubmitInput / ConsumeInputs /
// RequestCompletionPermit) over a throwaway Postgres; skipped unless UZI_TEST_DATABASE_URL is
// set (./e2e/run-store-it.sh). They reuse interlockLiveDB and permitService.

const milestonelessPlan = "# Plan\n\nOne focused change, no milestone breakdown.\n"

// seedOwnedRun inserts an issue run owned by workerID at `status`. interlocked stamps
// completion_contract_version=1; autopilot marks it auto_approve + plan_source='agent' (the
// state SetRunAutopilotPlan's guard admits).
func (e interlockLiveDB) seedOwnedRun(t *testing.T, workerID uuid.UUID, status string, interlocked, autopilot bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	iid := *e.nextIID
	*e.nextIID++
	var cv any
	if interlocked {
		cv = int32(1)
	}
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id,
	               completion_contract_version, auto_approve, plan_source)
	           VALUES ($1, $2, $3, $4, 't', 'd', 'issue', $5, $6, $7, $8, 'agent')`,
		id, e.userID, e.repoID, iid, status, workerID, cv, autopilot)
	return id
}

// milestoneColumns reads the raw candidate/frozen jsonb (nil = SQL NULL).
func (e interlockLiveDB) milestoneColumns(t *testing.T, runID uuid.UUID) (candidate, frozen []byte) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx, `SELECT milestones_candidate, milestones_frozen FROM runs WHERE id = $1`, runID).
		Scan(&candidate, &frozen); err != nil {
		t.Fatalf("read milestone columns: %v", err)
	}
	return candidate, frozen
}

// assertEmptyCriteriaContract asserts the frozen contract is non-NULL with criteria:[] (not null)
// at contract_revision 1.
func (e interlockLiveDB) assertEmptyCriteriaContract(t *testing.T, runID uuid.UUID) {
	t.Helper()
	contract, rev := e.readContract(t, runID)
	if contract == nil {
		t.Fatal("no completion contract froze for the interlocked milestone-less run (issue #1626: it must freeze criteria:[])")
	}
	if rev == nil || *rev != 1 {
		t.Fatalf("contract_revision = %v, want 1", rev)
	}
	var c struct {
		Criteria json.RawMessage `json:"criteria"`
	}
	if err := json.Unmarshal(contract, &c); err != nil {
		t.Fatalf("decode contract %s: %v", contract, err)
	}
	if string(c.Criteria) != "[]" {
		t.Fatalf("contract criteria = %s, want [] (contract %s)", c.Criteria, contract)
	}
}

// grantAndComplete requests the permit at revision 1 and completes the run with it.
func (e interlockLiveDB) grantAndComplete(t *testing.T, svc *Service, wkr store.Worker, runID uuid.UUID) {
	t.Helper()
	const head, branch = "0123abcd", "agent/issue-1626"
	permit, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: branch, Head: head})
	if err != nil || !permit.Granted {
		t.Fatalf("a milestone-less interlocked run's permit must be granted vacuously: res=%+v err=%v", permit, err)
	}
	run, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "completed", Head: strPtr(head), Branch: strPtr(branch)})
	if err != nil {
		t.Fatalf("SetState completed: %v", err)
	}
	if !applied || run.Status != "completed" {
		t.Fatalf("a permitted completion must terminate the run; applied=%v status=%q", applied, run.Status)
	}
}

// TestMilestonelessPlanFreezesAtApprovalLiveDB (issue #1626 (a)): a human-gated interlocked run
// parks at awaiting_approval with plan_md and NO milestones; the candidate is the explicit `[]`,
// the approve freezes criteria:[] at revision 1, the post-approval running report's returned row
// (the /state ack the worker reads completion_revision off) carries revision 1, and the permit
// grants so the run completes.
func TestMilestonelessPlanFreezesAtApprovalLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}
	runID := e.seedOwnedRun(t, wid, "running", true, false)

	if _, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "awaiting_approval", PlanMd: strPtr(milestonelessPlan)}); err != nil || !applied {
		t.Fatalf("SetState awaiting_approval: applied=%v err=%v", applied, err)
	}
	if cand, _ := e.milestoneColumns(t, runID); string(cand) != "[]" {
		t.Fatalf("milestones_candidate = %q, want [] for an interlocked milestone-less plan", cand)
	}

	if _, err := svc.SubmitInput(e.ctx, e.userID, runID, "approve_plan", "", &AgentSelection{Source: AgentSourceOwn}); err != nil {
		t.Fatalf("SubmitInput approve_plan: %v", err)
	}
	if _, frozen := e.milestoneColumns(t, runID); string(frozen) != "[]" {
		t.Fatalf("milestones_frozen = %q, want [] after approve", frozen)
	}
	e.assertEmptyCriteriaContract(t, runID)

	// The worker consumes the verdict and reports running: the returned row is what the /state
	// ack's RunDTO.completion_revision is built from.
	if _, err := svc.ConsumeInputs(e.ctx, wkr, runID); err != nil {
		t.Fatalf("ConsumeInputs: %v", err)
	}
	run, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "running"})
	if err != nil || !applied {
		t.Fatalf("SetState running after approve: applied=%v err=%v", applied, err)
	}
	if !run.ContractRevision.Valid || run.ContractRevision.Int32 != 1 {
		t.Fatalf("post-approval running ack row contract_revision = %+v, want 1", run.ContractRevision)
	}
	e.grantAndComplete(t, svc, wkr, runID)
}

// TestMilestonelessAutopilotFreezesAtPlanReportLiveDB (issue #1626 (b)): an interlocked AUTOPILOT
// run's claim-time running report (no plan_md) freezes NOTHING — the milestone-source guard stays
// load-bearing — and the plan-bearing running report (plan_md, no milestones) freezes `[]` +
// criteria:[] at revision 1, returned on that same report's row; the permit then grants.
func TestMilestonelessAutopilotFreezesAtPlanReportLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}
	runID := e.seedOwnedRun(t, wid, "claimed", true, true)

	// Claim-time report: no plan_md, no milestones — must not freeze (nor spend the freeze).
	run, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "running"})
	if err != nil || !applied {
		t.Fatalf("SetState running (claim-time): applied=%v err=%v", applied, err)
	}
	if _, frozen := e.milestoneColumns(t, runID); frozen != nil {
		t.Fatalf("claim-time running report froze milestones %q; only a plan-bearing report may", frozen)
	}
	if c, r := e.readContract(t, runID); c != nil || r != nil {
		t.Fatalf("claim-time running report froze a contract (%s, rev %v)", c, r)
	}
	if run.ContractRevision.Valid {
		t.Fatalf("claim-time ack row carries contract_revision %d before any freeze", run.ContractRevision.Int32)
	}

	// The autopilot plan report: plan_md, no milestones.
	run, applied, err = svc.SetState(e.ctx, wkr, runID, StateRequest{State: "running", PlanMd: strPtr(milestonelessPlan)})
	if err != nil || !applied {
		t.Fatalf("SetState running (plan report): applied=%v err=%v", applied, err)
	}
	if _, frozen := e.milestoneColumns(t, runID); string(frozen) != "[]" {
		t.Fatalf("milestones_frozen = %q, want [] after the milestone-less plan report", frozen)
	}
	e.assertEmptyCriteriaContract(t, runID)
	if !run.ContractRevision.Valid || run.ContractRevision.Int32 != 1 {
		t.Fatalf("plan-report ack row contract_revision = %+v, want 1 (the freezing report's own ack)", run.ContractRevision)
	}
	e.grantAndComplete(t, svc, wkr, runID)
}

// TestMilestonelessLegacyRunStaysNullLiveDB (issue #1626 (c)): a LEGACY (unstamped) run given the
// same milestone-less plan reports keeps milestones_candidate / milestones_frozen NULL and freezes
// no contract — byte-identical to before.
func TestMilestonelessLegacyRunStaysNullLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}

	gated := e.seedOwnedRun(t, wid, "running", false, false)
	if _, applied, err := svc.SetState(e.ctx, wkr, gated, StateRequest{State: "awaiting_approval", PlanMd: strPtr(milestonelessPlan)}); err != nil || !applied {
		t.Fatalf("SetState awaiting_approval (legacy): applied=%v err=%v", applied, err)
	}
	if cand, _ := e.milestoneColumns(t, gated); cand != nil {
		t.Fatalf("legacy milestones_candidate = %q, want NULL", cand)
	}
	if _, err := svc.SubmitInput(e.ctx, e.userID, gated, "approve_plan", "", &AgentSelection{Source: AgentSourceOwn}); err != nil {
		t.Fatalf("SubmitInput approve_plan (legacy): %v", err)
	}
	if cand, frozen := e.milestoneColumns(t, gated); cand != nil || frozen != nil {
		t.Fatalf("legacy approve wrote milestones (candidate %q, frozen %q), want NULL", cand, frozen)
	}
	if c, r := e.readContract(t, gated); c != nil || r != nil {
		t.Fatalf("legacy approve froze a contract (%s, rev %v)", c, r)
	}

	auto := e.seedOwnedRun(t, wid, "claimed", false, true)
	if _, applied, err := svc.SetState(e.ctx, wkr, auto, StateRequest{State: "running", PlanMd: strPtr(milestonelessPlan)}); err != nil || !applied {
		t.Fatalf("SetState running plan report (legacy): applied=%v err=%v", applied, err)
	}
	if cand, frozen := e.milestoneColumns(t, auto); cand != nil || frozen != nil {
		t.Fatalf("legacy autopilot plan report wrote milestones (candidate %q, frozen %q), want NULL", cand, frozen)
	}
	if c, r := e.readContract(t, auto); c != nil || r != nil {
		t.Fatalf("legacy autopilot plan report froze a contract (%s, rev %v)", c, r)
	}
}

// TestRepresentedGateKeepsCandidateLiveDB (issue #1626 B1): a reclaim at the plan gate (a
// credential switch or requeue while parked) re-presents the SAME plan with no milestones —
// assembleClaim replayed only the (still NULL) frozen list, so the worker had none to send. The
// re-presented report must NOT downgrade the stored non-empty candidate to `[]`: approval then
// freezes a 2-criterion contract and the permit is DENIED while m1/m2 are undone. Before the fix
// the direct-assignment candidate write stored `[]`, the approve froze criteria:[], and the
// permit was granted vacuously against a two-milestone plan.
func TestRepresentedGateKeepsCandidateLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}
	runID := e.seedOwnedRun(t, wid, "running", true, false)

	const plan = "# Plan\n\nTwo milestones.\n"
	ms := []Milestone{{ID: "m1", Title: "First"}, {ID: "m2", Title: "Second"}}
	if _, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "awaiting_approval", PlanMd: strPtr(plan), Milestones: &ms}); err != nil || !applied {
		t.Fatalf("SetState awaiting_approval (with milestones): applied=%v err=%v", applied, err)
	}
	// The re-presented gate: same plan_md, milestones absent.
	if _, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "awaiting_approval", PlanMd: strPtr(plan)}); err != nil || !applied {
		t.Fatalf("SetState awaiting_approval (re-presented): applied=%v err=%v", applied, err)
	}
	cand, _ := e.milestoneColumns(t, runID)
	if got, err := DecodeMilestones(cand); err != nil || len(got) != 2 || got[0].ID != "m1" || got[1].ID != "m2" {
		t.Fatalf("milestones_candidate = %s (err %v), want the original m1,m2 (a re-presented gate must not downgrade it)", cand, err)
	}

	if _, err := svc.SubmitInput(e.ctx, e.userID, runID, "approve_plan", "", &AgentSelection{Source: AgentSourceOwn}); err != nil {
		t.Fatalf("SubmitInput approve_plan: %v", err)
	}
	contract, rev := e.readContract(t, runID)
	if contract == nil || rev == nil || *rev != 1 {
		t.Fatalf("approve froze contract %s rev %v, want a revision-1 contract", contract, rev)
	}
	var c struct {
		Criteria []json.RawMessage `json:"criteria"`
	}
	if err := json.Unmarshal(contract, &c); err != nil {
		t.Fatalf("decode contract %s: %v", contract, err)
	}
	if len(c.Criteria) != 2 {
		t.Fatalf("contract has %d criteria, want 2 (contract %s)", len(c.Criteria), contract)
	}

	if _, err := svc.ConsumeInputs(e.ctx, wkr, runID); err != nil {
		t.Fatalf("ConsumeInputs: %v", err)
	}
	if _, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "running"}); err != nil || !applied {
		t.Fatalf("SetState running after approve: applied=%v err=%v", applied, err)
	}
	permit, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: "agent/issue-1626", Head: "0123abcd"})
	if err != nil {
		t.Fatalf("RequestCompletionPermit: %v", err)
	}
	if permit.Granted {
		t.Fatalf("permit granted with m1/m2 undone (vacuous grant against a two-milestone plan): %+v", permit)
	}
}

// TestMilestonelessReviseDropsCandidateLiveDB (issue #1626 review, F2): the first gate presents
// plan A with two valid milestones (candidate m1,m2); a revise then presents a DIFFERENT plan B
// with NO milestones (the worker omits an empty list). The superseded plan's criteria must not
// survive: the candidate resets to `[]`, the approve freezes `[]` + criteria:[] at revision 1, and
// the permit grants. Before the fix the candidate kept m1,m2, the approve froze them, and the
// permit was denied for milestones the approved plan no longer has.
func TestMilestonelessReviseDropsCandidateLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}
	runID := e.seedOwnedRun(t, wid, "running", true, false)

	ms := []Milestone{{ID: "m1", Title: "First"}, {ID: "m2", Title: "Second"}}
	if _, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "awaiting_approval", PlanMd: strPtr("# Plan A\n\nTwo milestones.\n"), Milestones: &ms}); err != nil || !applied {
		t.Fatalf("SetState awaiting_approval (plan A, with milestones): applied=%v err=%v", applied, err)
	}
	if cand, _ := e.milestoneColumns(t, runID); len(cand) == 0 || string(cand) == "[]" {
		t.Fatalf("milestones_candidate = %q, want plan A's m1,m2", cand)
	}

	if _, err := svc.SubmitInput(e.ctx, e.userID, runID, "revise_plan", "drop the milestones", nil); err != nil {
		t.Fatalf("SubmitInput revise_plan: %v", err)
	}
	if _, err := svc.ConsumeInputs(e.ctx, wkr, runID); err != nil {
		t.Fatalf("ConsumeInputs (revise): %v", err)
	}
	if _, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "awaiting_approval", PlanMd: strPtr(milestonelessPlan)}); err != nil || !applied {
		t.Fatalf("SetState awaiting_approval (plan B, no milestones): applied=%v err=%v", applied, err)
	}
	if cand, _ := e.milestoneColumns(t, runID); string(cand) != "[]" {
		t.Fatalf("milestones_candidate = %q after a milestone-less revise to a different plan, want [] (the superseded plan's m1,m2 must not survive)", cand)
	}

	if _, err := svc.SubmitInput(e.ctx, e.userID, runID, "approve_plan", "", &AgentSelection{Source: AgentSourceOwn}); err != nil {
		t.Fatalf("SubmitInput approve_plan: %v", err)
	}
	if _, frozen := e.milestoneColumns(t, runID); string(frozen) != "[]" {
		t.Fatalf("milestones_frozen = %q, want [] after approving the milestone-less plan B", frozen)
	}
	e.assertEmptyCriteriaContract(t, runID)

	if _, err := svc.ConsumeInputs(e.ctx, wkr, runID); err != nil {
		t.Fatalf("ConsumeInputs (approve): %v", err)
	}
	if _, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "running"}); err != nil || !applied {
		t.Fatalf("SetState running after approve: applied=%v err=%v", applied, err)
	}
	e.grantAndComplete(t, svc, wkr, runID)
}

// TestRejectedListThenMilestonelessReviseHoldsLiveDB (issue #1626 B-a): an interlocked run's first
// plan-bearing report carries a milestone list the server REJECTS (a duplicate id, which the
// worker's blank-field check does not catch), so the candidate is NULL beside a stored plan_md.
// A revise then reports a DIFFERENT plan with no milestones. That NULL must stay sticky: the
// revised report must not infer the vacuous `[]`, the approve must freeze NO contract, and the
// permit must not be granted — the run holds at finalize, where the owner's completion decision
// is the escape. Before the fix the same-plan-text guard missed the different plan, the candidate
// became `[]`, approve froze criteria:[], and the permit was granted.
func TestRejectedListThenMilestonelessReviseHoldsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}
	runID := e.seedOwnedRun(t, wid, "running", true, false)

	dup := []Milestone{{ID: "m1", Title: "First"}, {ID: "m1", Title: "Second"}}
	if _, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "awaiting_approval", PlanMd: strPtr("# Plan A"), Milestones: &dup}); err != nil || !applied {
		t.Fatalf("SetState awaiting_approval (rejected list): applied=%v err=%v", applied, err)
	}
	if cand, _ := e.milestoneColumns(t, runID); cand != nil {
		t.Fatalf("milestones_candidate = %q, want NULL (a duplicate-id list is rejected)", cand)
	}

	if _, err := svc.SubmitInput(e.ctx, e.userID, runID, "revise_plan", "split it differently", nil); err != nil {
		t.Fatalf("SubmitInput revise_plan: %v", err)
	}
	if _, err := svc.ConsumeInputs(e.ctx, wkr, runID); err != nil {
		t.Fatalf("ConsumeInputs (revise): %v", err)
	}
	if _, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "awaiting_approval", PlanMd: strPtr("# Plan B (revised)")}); err != nil || !applied {
		t.Fatalf("SetState awaiting_approval (revised, no milestones): applied=%v err=%v", applied, err)
	}
	if cand, _ := e.milestoneColumns(t, runID); cand != nil {
		t.Fatalf("milestones_candidate = %q after a milestone-less revise over a rejected list, want NULL (sticky rejection)", cand)
	}

	if _, err := svc.SubmitInput(e.ctx, e.userID, runID, "approve_plan", "", &AgentSelection{Source: AgentSourceOwn}); err != nil {
		t.Fatalf("SubmitInput approve_plan: %v", err)
	}
	if c, r := e.readContract(t, runID); c != nil || r != nil {
		t.Fatalf("approve froze a contract (%s, rev %v) over a rejected milestone list, want none", c, r)
	}
	if _, err := svc.ConsumeInputs(e.ctx, wkr, runID); err != nil {
		t.Fatalf("ConsumeInputs (approve): %v", err)
	}
	if _, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "running"}); err != nil || !applied {
		t.Fatalf("SetState running after approve: applied=%v err=%v", applied, err)
	}
	if c, r := e.readContract(t, runID); c != nil || r != nil {
		t.Fatalf("the post-approval running report froze a contract (%s, rev %v), want none", c, r)
	}
	permit, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: "agent/issue-1626", Head: "0123abcd"})
	if err != nil {
		t.Fatalf("RequestCompletionPermit: %v", err)
	}
	if permit.Granted {
		t.Fatalf("permit granted after a rejected milestone list and a milestone-less revise (vacuous grant): %+v", permit)
	}
}

// TestAutopilotRejectedListThenMilestonelessReportHoldsLiveDB is B-a's autopilot twin: the first
// plan-bearing `running` report carries a rejected list (frozen stays NULL, SetRunAutopilotPlan
// stores plan_md), and a later plan-bearing report of the same plan with no milestones must not
// freeze `[]` — no contract, no grant.
func TestAutopilotRejectedListThenMilestonelessReportHoldsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}
	runID := e.seedOwnedRun(t, wid, "claimed", true, true)

	dup := []Milestone{{ID: "m1", Title: "First"}, {ID: "m1", Title: "Second"}}
	if _, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "running", PlanMd: strPtr(milestonelessPlan), Milestones: &dup}); err != nil || !applied {
		t.Fatalf("SetState running (plan report, rejected list): applied=%v err=%v", applied, err)
	}
	if c, r := e.readContract(t, runID); c != nil || r != nil {
		t.Fatalf("a rejected list froze a contract (%s, rev %v)", c, r)
	}
	if _, applied, err := svc.SetState(e.ctx, wkr, runID, StateRequest{State: "running", PlanMd: strPtr(milestonelessPlan)}); err != nil || !applied {
		t.Fatalf("SetState running (plan re-report, no milestones): applied=%v err=%v", applied, err)
	}
	if _, frozen := e.milestoneColumns(t, runID); frozen != nil {
		t.Fatalf("milestones_frozen = %q after a rejected list, want NULL (sticky rejection)", frozen)
	}
	if c, r := e.readContract(t, runID); c != nil || r != nil {
		t.Fatalf("a milestone-less re-report after a rejected list froze a contract (%s, rev %v)", c, r)
	}
	permit, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: "agent/issue-1626", Head: "0123abcd"})
	if err != nil {
		t.Fatalf("RequestCompletionPermit: %v", err)
	}
	if permit.Granted {
		t.Fatalf("autopilot permit granted after a rejected milestone list (vacuous grant): %+v", permit)
	}
}

// planMdStored reports whether the run's plan_md column is non-NULL.
func (e interlockLiveDB) planMdStored(t *testing.T, runID uuid.UUID) bool {
	t.Helper()
	var stored bool
	if err := e.pool.QueryRow(e.ctx, `SELECT plan_md IS NOT NULL FROM runs WHERE id = $1`, runID).Scan(&stored); err != nil {
		t.Fatalf("read plan_md: %v", err)
	}
	return stored
}

// failClaimedToRunning installs a Postgres trigger that makes the claimed -> running UPDATE of
// runID raise, so SetRunRunning fails at the real DB boundary while SetRunAutopilotPlan (which
// keeps the status) still succeeds. The returned func drops it; t.Cleanup drops it too.
func (e interlockLiveDB) failClaimedToRunning(t *testing.T, runID uuid.UUID) func() {
	t.Helper()
	suffix := strings.ReplaceAll(runID.String(), "-", "")
	fn, trg := "uzi_test_fail_running_"+suffix, "uzi_test_fail_running_trg_"+suffix
	e.exec(t, `CREATE FUNCTION `+fn+`() RETURNS trigger LANGUAGE plpgsql AS $$
	           BEGIN
	             IF OLD.status = 'claimed' AND NEW.status = 'running' THEN
	               RAISE EXCEPTION 'issue #1626 test: injected SetRunRunning failure';
	             END IF;
	             RETURN NEW;
	           END $$`)
	e.exec(t, `CREATE TRIGGER `+trg+` BEFORE UPDATE ON runs FOR EACH ROW
	           WHEN (OLD.id = '`+runID.String()+`'::uuid) EXECUTE FUNCTION `+fn+`()`)
	drop := func() {
		_, _ = e.pool.Exec(e.ctx, `DROP TRIGGER IF EXISTS `+trg+` ON runs`)
		_, _ = e.pool.Exec(e.ctx, `DROP FUNCTION IF EXISTS `+fn+`()`)
	}
	t.Cleanup(drop)
	return drop
}

// TestAutopilotPlanWriteAtomicWithRunningLiveDB (issue #1626 review): an UNFENCED (nil
// claim_generation) autopilot plan report whose SetRunRunning fails must not leave plan_md
// committed on its own. If it did, planMilestonesParam would read the worker's retry of that same
// report as a sticky rejection (stored plan_md, NULL candidate), freeze nothing, and the run would
// hold at finalize with contract_not_frozen. The plan write and SetRunRunning share one tx, so
// the failed report rolls back plan_md and the retry freezes `[]` + criteria:[].
func TestAutopilotPlanWriteAtomicWithRunningLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}
	runID := e.seedOwnedRun(t, wid, "claimed", true, true)

	drop := e.failClaimedToRunning(t, runID)
	report := StateRequest{State: "running", PlanMd: strPtr(milestonelessPlan)} // no ClaimGeneration: unfenced
	if _, _, err := svc.SetState(e.ctx, wkr, runID, report); err == nil {
		t.Fatal("SetState running succeeded despite the injected SetRunRunning failure")
	}
	if e.planMdStored(t, runID) {
		t.Fatal("plan_md committed although SetRunRunning failed; the retry would read it as a sticky rejection")
	}
	if got := e.runStatus(t, runID); got != "claimed" {
		t.Fatalf("status = %q after the failed report, want claimed", got)
	}

	drop()
	run, applied, err := svc.SetState(e.ctx, wkr, runID, report)
	if err != nil || !applied {
		t.Fatalf("SetState running (retry): applied=%v err=%v", applied, err)
	}
	if _, frozen := e.milestoneColumns(t, runID); string(frozen) != "[]" {
		t.Fatalf("milestones_frozen = %q after the retried plan report, want []", frozen)
	}
	e.assertEmptyCriteriaContract(t, runID)
	if !run.ContractRevision.Valid || run.ContractRevision.Int32 != 1 {
		t.Fatalf("retry ack row contract_revision = %+v, want 1", run.ContractRevision)
	}
}

// TestUnfencedPlanRefusalReadsOutsideTxLiveDB: the planRows == 0 refusal branch re-reads the run
// on the pool while the unfenced plan tx is open. A human-gated (non-autopilot) run makes
// SetRunAutopilotPlan match no row; the report must come back promptly as ErrInvalidState (no
// wait on the open tx) with nothing stored. A behavioural pin, not a mutation guard: it passes on
// the pre-#1626 code too, since a pool read never waits on a row lock and a 0-row UPDATE holds none.
func TestUnfencedPlanRefusalReadsOutsideTxLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	wkr := store.Worker{ID: wid}
	runID := e.seedOwnedRun(t, wid, "claimed", true, false)

	ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
	defer cancel()
	_, applied, err := svc.SetState(ctx, wkr, runID, StateRequest{State: "running", PlanMd: strPtr(milestonelessPlan)})
	if ctx.Err() != nil {
		t.Fatalf("refusal branch blocked until the deadline: %v", err)
	}
	if !errors.Is(err, ErrInvalidState) || applied {
		t.Fatalf("human-gated run's plan report: applied=%v err=%v, want ErrInvalidState", applied, err)
	}
	if e.planMdStored(t, runID) {
		t.Fatal("a refused plan report stored plan_md")
	}
	if got := e.runStatus(t, runID); got != "claimed" {
		t.Fatalf("status = %q after a refused plan report, want claimed", got)
	}
}
