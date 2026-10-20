package workersvc

import (
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// The omitted-M5 regression (PRD #1230 M5): a coverage report that leaves a source
// requirement unrepresented must not be complete under the single server authority, so the
// gate projection requires a human; the fixed (represented) report is complete and needs
// none. Removing the represented comparison from evaluateSemanticCoverage admits the
// omitted plan and turns the first case red.
func TestSemanticCoverageOmittedRequirementIsNotComplete(t *testing.T) {
	for _, tc := range []struct {
		name        string
		m5Status    string
		wantOutcome string
		wantPark    string
		humanNeeded bool
	}{
		{"omitted M5 requirement", "missing", "incomplete", semanticParkCoverageIncomplete, true},
		{"uncertain M5 requirement", "uncertain", "incomplete", semanticParkCoverageIncomplete, true},
		{"fixed plan represents M5", "represented", "complete", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, run, _ := authorityFixture("complete", true, 7, uuid.Nil)
			report := c.ledger.Reviews[0].Report
			report.Requirements = append(report.Requirements, apitypes.SemanticCoverageResult{RequirementID: "r2", Status: tc.m5Status})
			got := evaluateSemanticCoverage(c, run)
			if got.Status.Outcome != tc.wantOutcome {
				t.Fatalf("authority outcome=%q, want %q", got.Status.Outcome, tc.wantOutcome)
			}
			projection := semanticGateProjection(c, run)
			if projection.Coverage.Outcome != tc.wantOutcome {
				t.Fatalf("gate projection outcome=%q, want %q (single authority)", projection.Coverage.Outcome, tc.wantOutcome)
			}
			park := expectedSemanticPark(&projection)
			if park != tc.wantPark || (park != "") != tc.humanNeeded {
				t.Fatalf("park=%q, want %q (human needed=%v)", park, tc.wantPark, tc.humanNeeded)
			}
		})
	}
}
