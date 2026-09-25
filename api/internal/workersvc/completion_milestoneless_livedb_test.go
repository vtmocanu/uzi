package workersvc

import (
	"encoding/json"
	"testing"

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
