package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// The worker status and the gate projection both derive from evaluateSemanticCoverage.
// This drives real persisted reviews (omitted M5 first, then the fixed plan) and requires
// SemanticCoverageStatus and the loaded gate projection to agree at every step.
func TestSemanticCoverageOmittedRequirementAuthorityLiveDB(t *testing.T) {
	f := newCoverageLiveFixture(t)
	f.svc.q = f.e.q
	f.sources = f.sources[:1]
	f.submit(t)
	if f.candidate.SourceOutcome != "complete" {
		t.Fatalf("fixture must be fully captured, got %s", f.candidate.SourceOutcome)
	}
	criterion := f.candidate.Criteria[0]
	publish := func(m5 apitypes.SemanticCoverageResult) {
		t.Helper()
		reservation := f.reserve(t, "")
		if reservation.Outcome != "reserved" {
			t.Fatalf("reservation=%+v", reservation)
		}
		used := 10
		f.settle(t, reservation, "success", &used)
		report := apitypes.SemanticCoverageReport{
			SemanticCoverageBinding: SemanticCoverageBinding(f.candidate),
			Requirements: []apitypes.SemanticCoverageResult{
				{RequirementID: "r1", Status: "represented", Explanation: "behavior represented", Citations: criterion.Citations, CriterionIDs: []string{criterion.ID}},
				m5,
			},
			Criteria: []apitypes.SemanticCoverageAtomicity{{CriterionID: criterion.ID, Atomic: true, Explanation: "atomic behavior"}},
		}
		raw := semanticJSON(t, apitypes.SemanticCoveragePublishRequest{ReviewID: reservation.ReviewID, InvocationID: reservation.InvocationID, Report: report})
		if err := f.svc.PublishSemanticCoverage(f.e.ctx, f.worker, f.run, 7, raw); err != nil {
			t.Fatal(err)
		}
	}
	agree := func(wantOutcome, wantPark string) {
		t.Helper()
		status := f.status(t)
		run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
		if err != nil {
			t.Fatal(err)
		}
		projection, _, err := loadSemanticGate(f.e.ctx, f.e.q, run)
		if err != nil || projection == nil {
			t.Fatalf("gate projection: %v", err)
		}
		if status.Outcome != wantOutcome || projection.Coverage.Outcome != wantOutcome {
			t.Fatalf("status=%q projection=%q, want both %q", status.Outcome, projection.Coverage.Outcome, wantOutcome)
		}
		if park := expectedSemanticPark(projection); park != wantPark {
			t.Fatalf("park=%q, want %q", park, wantPark)
		}
	}
	omitted := apitypes.SemanticCoverageResult{RequirementID: "r2", Status: "missing", Explanation: "M5 is not planned", Citations: criterion.Citations}
	publish(omitted)
	agree("incomplete", semanticParkCoverageIncomplete)
	fixed := apitypes.SemanticCoverageResult{RequirementID: "r2", Status: "represented", Explanation: "M5 is planned", Citations: criterion.Citations, CriterionIDs: []string{criterion.ID}}
	publish(fixed)
	agree("complete", "")
}
