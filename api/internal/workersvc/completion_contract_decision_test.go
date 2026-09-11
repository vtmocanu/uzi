package workersvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These are the Go-decision counterpart to completion_interlock_livedb_test.go. The LiveDB
// tests prove the SQL freeze GUARD (the query writes the contract once, idempotently, only for
// an interlocked run) by calling CreateApprovePlanInput / SetRunRunning DIRECTLY with a
// pre-built contract. That leaves the PRODUCTION Go DECISION — the
// `if run.CompletionContractVersion.Valid && len(run.CompletionContract) == 0` guard in
// submitApproval (submit.go) and runningStateParams (service.go) that decides WHETHER to build
// a contract at all — untested: neutering both guards to `if false && ...` passed the whole
// suite. These fake-store unit tests close that gap by driving the real public entry points
// (SubmitInput / SetState) and asserting on the CompletionContract the production code puts on
// the params it hands the store, so a regression that disables contract-freezing reddens here.

// interlockVersion is the completion_contract_version an interlocked run carries (D1: 1).
func interlockVersion() pgtype.Int4 { return pgtype.Int4{Int32: 1, Valid: true} }

// milestonesJSON encodes a milestone list the way runs.milestones_candidate / _frozen store it.
func milestonesJSON(t *testing.T, ms ...Milestone) []byte {
	t.Helper()
	raw, err := json.Marshal(ms)
	if err != nil {
		t.Fatalf("marshal milestones: %v", err)
	}
	return raw
}

// assertStructuralContract proves the captured param carries a well-formed structural contract
// the PRODUCTION code built: the D1 wire shape {"profile":"structural","revision":1,
// "criteria":[...]} with exactly one criterion per expected milestone, each criterion's
// id/milestone_id/text derived from its milestone and the reserved audit/finding_ids slots in
// their D1 shape (null / []). This is the assertion that reddens when a freeze guard is
// disabled (the param goes nil).
func assertStructuralContract(t *testing.T, got []byte, want []Milestone) {
	t.Helper()
	if got == nil {
		t.Fatal("CompletionContract is nil: the production freeze guard did not build a contract")
	}
	// The reserved slots must be on the wire as `null` / `[]` (the migration comment and
	// #1230/#1231 depend on the exact shape); a decode alone cannot tell `[]` from `null`.
	if !strings.Contains(string(got), `"audit":null`) {
		t.Errorf("contract missing `\"audit\":null`: %s", got)
	}
	if !strings.Contains(string(got), `"finding_ids":[]`) {
		t.Errorf("contract missing `\"finding_ids\":[]`: %s", got)
	}
	var c completionContract
	if err := json.Unmarshal(got, &c); err != nil {
		t.Fatalf("contract is not well-formed JSON: %v (raw %s)", err, got)
	}
	if c.Profile != contractProfileStructural {
		t.Errorf("profile = %q, want %q", c.Profile, contractProfileStructural)
	}
	if c.Revision != contractRevisionInitial {
		t.Errorf("revision = %d, want %d", c.Revision, contractRevisionInitial)
	}
	if len(c.Criteria) != len(want) {
		t.Fatalf("criteria = %d, want one per milestone (%d)", len(c.Criteria), len(want))
	}
	for i, m := range want {
		cr := c.Criteria[i]
		if cr.MilestoneID != m.ID {
			t.Errorf("criterion[%d].milestone_id = %q, want %q", i, cr.MilestoneID, m.ID)
		}
		if cr.ID != m.ID+".c1" {
			t.Errorf("criterion[%d].id = %q, want %q", i, cr.ID, m.ID+".c1")
		}
		if cr.Text != m.Title {
			t.Errorf("criterion[%d].text = %q, want %q", i, cr.Text, m.Title)
		}
		if cr.Audit != nil {
			t.Errorf("criterion[%d].audit = %v, want nil (structural profile has no evidence)", i, cr.Audit)
		}
		if cr.FindingIDs == nil || len(cr.FindingIDs) != 0 {
			t.Errorf("criterion[%d].finding_ids = %v, want empty non-nil", i, cr.FindingIDs)
		}
	}
}

// -------------------------------------------------------------------------
// Approve path: submitApproval, driven through the real SubmitInput entry.
// -------------------------------------------------------------------------

// TestSubmitApprovalBuildsCompletionContract is the positive approve-path decision test: an
// interlocked run (completion_contract_version=1) with an unfrozen contract and a candidate
// milestone list, approved through SubmitInput, must have the SERVER build the structural
// contract and hand it to CreateApprovePlanInput. submitApproval reads the milestone SOURCE
// from the freeze snapshot (GetRunMilestoneFreezeSnapshot), so the fixture seeds the snapshot's
// candidate list, mirroring what the freeze query COALESCEs a line later.
func TestSubmitApprovalBuildsCompletionContract(t *testing.T) {
	ms := []Milestone{{ID: "m1", Title: "First milestone"}, {ID: "m2", Title: "Second milestone"}}
	runID := uuid.New()
	fs := &fakeStore{
		runByID: store.Run{
			ID:                        runID,
			Status:                    "awaiting_approval",
			CompletionContractVersion: interlockVersion(),
			// CompletionContract left nil: the contract is not yet frozen.
		},
		freezeSnapshot: store.GetRunMilestoneFreezeSnapshotRow{MilestonesCandidate: milestonesJSON(t, ms...)},
	}
	svc := New(fs, newBox(t), testParams())

	sel := &AgentSelection{Source: AgentSourceOwn}
	if _, err := svc.SubmitInput(context.Background(), uuid.New(), runID, "approve_plan", "", sel); err != nil {
		t.Fatalf("SubmitInput(approve_plan): %v", err)
	}
	if fs.createdApproval == nil {
		t.Fatal("CreateApprovePlanInput was not called")
	}
	assertStructuralContract(t, fs.createdApproval.CompletionContract, ms)
}

// TestSubmitApprovalNoContractForLegacyRun is the negative (legacy) approve-path decision test:
// a run whose completion_contract_version is NULL must NOT get a Go-built contract even when a
// candidate list is present — the guard's version check keeps the param nil, so the run stays
// legacy.
func TestSubmitApprovalNoContractForLegacyRun(t *testing.T) {
	runID := uuid.New()
	fs := &fakeStore{
		runByID: store.Run{
			ID:     runID,
			Status: "awaiting_approval",
			// CompletionContractVersion left as the zero value: NOT valid ⇒ a legacy run.
		},
		// A candidate list IS present, so a wrongly-fired guard would build a non-nil contract.
		freezeSnapshot: store.GetRunMilestoneFreezeSnapshotRow{
			MilestonesCandidate: milestonesJSON(t, Milestone{ID: "m1", Title: "First"}),
		},
	}
	svc := New(fs, newBox(t), testParams())

	sel := &AgentSelection{Source: AgentSourceOwn}
	if _, err := svc.SubmitInput(context.Background(), uuid.New(), runID, "approve_plan", "", sel); err != nil {
		t.Fatalf("SubmitInput(approve_plan): %v", err)
	}
	if fs.createdApproval == nil {
		t.Fatal("CreateApprovePlanInput was not called")
	}
	if fs.createdApproval.CompletionContract != nil {
		t.Fatalf("legacy run built a contract (%s); a NULL version must never freeze", fs.createdApproval.CompletionContract)
	}
}

// TestSubmitApprovalNoRebuildWhenAlreadyFrozen is the idempotent approve-path decision test: an
// interlocked run whose contract is ALREADY frozen (completion_contract non-empty) must not have
// the Go side rebuild it. Production writes the local `completionContract` var, which the guard
// leaves nil in this case (it never copies the run's existing value) — the store's own COALESCE
// guard is what preserves the frozen contract — so the captured param is nil.
func TestSubmitApprovalNoRebuildWhenAlreadyFrozen(t *testing.T) {
	runID := uuid.New()
	// An already-frozen contract on the run, built from a DIFFERENT milestone list than the
	// snapshot's, so a wrongful rebuild would be detectable (and non-nil regardless).
	frozen, err := buildCompletionContract(milestonesJSON(t, Milestone{ID: "old", Title: "Already frozen"}))
	if err != nil {
		t.Fatalf("buildCompletionContract: %v", err)
	}
	fs := &fakeStore{
		runByID: store.Run{
			ID:                        runID,
			Status:                    "awaiting_approval",
			CompletionContractVersion: interlockVersion(),
			CompletionContract:        frozen,
		},
		freezeSnapshot: store.GetRunMilestoneFreezeSnapshotRow{
			MilestonesCandidate: milestonesJSON(t, Milestone{ID: "new", Title: "Would be rebuilt"}),
		},
	}
	svc := New(fs, newBox(t), testParams())

	sel := &AgentSelection{Source: AgentSourceOwn}
	if _, err := svc.SubmitInput(context.Background(), uuid.New(), runID, "approve_plan", "", sel); err != nil {
		t.Fatalf("SubmitInput(approve_plan): %v", err)
	}
	if fs.createdApproval == nil {
		t.Fatal("CreateApprovePlanInput was not called")
	}
	if fs.createdApproval.CompletionContract != nil {
		t.Fatalf("already-frozen run rebuilt a contract (%s); the Go guard must pass nil and defer to the store's idempotent guard", fs.createdApproval.CompletionContract)
	}
}

// -------------------------------------------------------------------------
// Running path: runningStateParams, driven through the real SetState entry.
// -------------------------------------------------------------------------

// TestRunningReportBuildsCompletionContract is the positive running-path decision test: an
// interlocked run with an unfrozen contract, reporting `running`, must have the SERVER build the
// structural contract and hand it to SetRunRunning. The running path resolves its milestone
// source from COALESCE(run.milestones_frozen, this report's frozen list, run.milestones_candidate);
// the fixture seeds run.milestones_candidate (the third source), which the task calls out.
func TestRunningReportBuildsCompletionContract(t *testing.T) {
	ms := []Milestone{{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"}}
	w := worker()
	fs := &fakeStore{
		runOwned: store.Run{
			ID:                        uuid.New(),
			WorkerID:                  pgconv.UUID(w.ID),
			Status:                    "running",
			CompletionContractVersion: interlockVersion(),
			MilestonesCandidate:       milestonesJSON(t, ms...),
			// CompletionContract left nil: not yet frozen.
		},
		setRunningRows: 1,
	}
	svc := New(fs, newBox(t), testParams())

	if _, _, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{
		State: "running", IterationCount: 1,
	}); err != nil {
		t.Fatalf("SetState(running): %v", err)
	}
	if fs.setRunningParams == nil {
		t.Fatal("SetRunRunning was not called")
	}
	assertStructuralContract(t, fs.setRunningParams.CompletionContract, ms)
}

// TestRunningReportNoContractForLegacyRun is the negative (legacy) running-path decision test: a
// run whose completion_contract_version is NULL must NOT get a Go-built contract on a `running`
// report even with a candidate list present.
func TestRunningReportNoContractForLegacyRun(t *testing.T) {
	w := worker()
	fs := &fakeStore{
		runOwned: store.Run{
			ID:       uuid.New(),
			WorkerID: pgconv.UUID(w.ID),
			Status:   "running",
			// CompletionContractVersion zero value: NOT valid ⇒ legacy.
			MilestonesCandidate: milestonesJSON(t, Milestone{ID: "m1", Title: "Alpha"}),
		},
		setRunningRows: 1,
	}
	svc := New(fs, newBox(t), testParams())

	if _, _, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{
		State: "running", IterationCount: 1,
	}); err != nil {
		t.Fatalf("SetState(running): %v", err)
	}
	if fs.setRunningParams == nil {
		t.Fatal("SetRunRunning was not called")
	}
	if fs.setRunningParams.CompletionContract != nil {
		t.Fatalf("legacy run built a contract (%s); a NULL version must never freeze", fs.setRunningParams.CompletionContract)
	}
}

// TestRunningReportNoRebuildWhenAlreadyFrozen is the idempotent running-path decision test: an
// interlocked run whose contract is already frozen must not have the Go side rebuild it on an
// ordinary heartbeat — the guard leaves the param nil and the store's COALESCE guard preserves
// the frozen contract.
func TestRunningReportNoRebuildWhenAlreadyFrozen(t *testing.T) {
	frozen, err := buildCompletionContract(milestonesJSON(t, Milestone{ID: "old", Title: "Already frozen"}))
	if err != nil {
		t.Fatalf("buildCompletionContract: %v", err)
	}
	w := worker()
	fs := &fakeStore{
		runOwned: store.Run{
			ID:                        uuid.New(),
			WorkerID:                  pgconv.UUID(w.ID),
			Status:                    "running",
			CompletionContractVersion: interlockVersion(),
			CompletionContract:        frozen,
			MilestonesCandidate:       milestonesJSON(t, Milestone{ID: "new", Title: "Would be rebuilt"}),
		},
		setRunningRows: 1,
	}
	svc := New(fs, newBox(t), testParams())

	if _, _, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{
		State: "running", IterationCount: 2,
	}); err != nil {
		t.Fatalf("SetState(running): %v", err)
	}
	if fs.setRunningParams == nil {
		t.Fatal("SetRunRunning was not called")
	}
	if fs.setRunningParams.CompletionContract != nil {
		t.Fatalf("already-frozen run rebuilt a contract (%s); the Go guard must pass nil and defer to the store's idempotent guard", fs.setRunningParams.CompletionContract)
	}
}
