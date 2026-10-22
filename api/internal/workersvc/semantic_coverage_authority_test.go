package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func authorityFixture(sourceOutcome string, atomic bool, invocationGeneration int64, invocationWorker uuid.UUID) (*semanticCoverageTx, store.Run, uuid.UUID) {
	worker := uuid.New()
	if invocationWorker == uuid.Nil {
		invocationWorker = worker
	}
	candidate := apitypes.SemanticCandidate{Digest: "digest", SourceOutcome: sourceOutcome}
	binding := SemanticCoverageBinding(candidate)
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	c := &semanticCoverageTx{
		candidate: candidate,
		policy:    apitypes.SemanticPolicy{Version: 1, Model: "sonnet", MaxReviews: 3, MaxInvocations: 2, MaxTokens: 120000, MaxSeconds: 900},
		ledger: semanticCoverageLedger{Version: 1, Reviews: []semanticCoverageReview{{
			ID: uuid.NewString(), Binding: binding, Started: start, Deadline: start.Add(time.Minute),
			Invocations: []semanticCoverageInvocation{{ID: uuid.NewString(), RequestID: uuid.NewString(), WorkerID: invocationWorker, Generation: invocationGeneration, Reserved: 100, Charged: 10, Status: "success"}},
			Report: &apitypes.SemanticCoverageReport{SemanticCoverageBinding: binding,
				Requirements: []apitypes.SemanticCoverageResult{{RequirementID: "r1", Status: "represented"}},
				Criteria:     []apitypes.SemanticCoverageAtomicity{{CriterionID: "c1", Atomic: atomic}}},
		}}},
	}
	run := store.Run{ClaimGeneration: 7, WorkerID: pgtype.UUID{Bytes: worker, Valid: true}}
	return c, run, worker
}

// The single coverage authority counts a report only when one of its invocations succeeded on the
// run's CURRENT claim generation and CURRENT worker. Both the gate presentation and the worker
// status read it, so a stale success from a reclaimed generation or another worker is never
// complete in either.
func TestSemanticCoverageAuthorityRequiresCurrentClaim(t *testing.T) {
	for _, tc := range []struct {
		name          string
		sourceOutcome string
		atomic        bool
		generation    int64
		otherWorker   bool
		want          string
	}{
		{"current success is complete", "complete", true, 7, false, "complete"},
		{"stale generation success is not complete", "complete", true, 6, false, "unavailable"},
		{"later generation success is not complete", "complete", true, 8, false, "unavailable"},
		{"other worker success is not complete", "complete", true, 7, true, "unavailable"},
		{"current non-atomic success is incomplete", "complete", false, 7, false, "incomplete"},
		{"current success on unknown sources is uncertain", "unknown", true, 7, false, "uncertain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := uuid.Nil
			if tc.otherWorker {
				other = uuid.New()
			}
			c, run, _ := authorityFixture(tc.sourceOutcome, tc.atomic, tc.generation, other)
			got := evaluateSemanticCoverage(c, run)
			if got.Status.Outcome != tc.want {
				t.Fatalf("authority outcome=%q, want %q", got.Status.Outcome, tc.want)
			}
			if projection := semanticGateProjection(c, run); projection.Coverage.Outcome != tc.want {
				t.Fatalf("gate projection outcome=%q, want %q (same authority)", projection.Coverage.Outcome, tc.want)
			}
			if tc.want != "complete" && tc.want != "incomplete" && tc.want != "uncertain" && got.Status.Report != nil {
				t.Fatal("a non-current report was surfaced as evidence")
			}
		})
	}
}

// With no counting report, a spent review, time or token budget is exhausted; otherwise the
// outcome is unavailable. An unreachable-budget status still collapses to unavailable on the
// worker wire (the gate presentation keeps exhausted).
func TestSemanticCoverageAuthorityExhaustion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(c *semanticCoverageTx)
		want   string
	}{
		{"nothing spent", func(*semanticCoverageTx) {}, "unavailable"},
		{"reviews spent", func(c *semanticCoverageTx) {
			r := c.ledger.Reviews[0]
			c.ledger.Reviews = []semanticCoverageReview{r, r, r}
		}, "exhausted"},
		{"tokens spent", func(c *semanticCoverageTx) { c.ledger.Reviews[0].Invocations[0].Charged = 120000 }, "exhausted"},
		{"time spent", func(c *semanticCoverageTx) {
			c.ledger.Reviews[0].Deadline = c.ledger.Reviews[0].Started.Add(15 * time.Minute)
		}, "exhausted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, run, _ := authorityFixture("complete", true, 6, uuid.Nil) // stale: no counting report
			tc.mutate(c)
			got := evaluateSemanticCoverage(c, run)
			if got.Status.Outcome != tc.want {
				t.Fatalf("outcome=%q, want %q", got.Status.Outcome, tc.want)
			}
			if expectedSemanticPark(&apitypes.SemanticGatePresentation{Coverage: got.Status}) != semanticParkCoverageUnavailable {
				t.Fatal("unavailable and exhausted must both park as coverage_unavailable")
			}
		})
	}
}

func TestExpectedSemanticPark(t *testing.T) {
	maintainers := []apitypes.SemanticMaintainerApproval{{ID: "m"}}
	for _, tc := range []struct {
		outcome     string
		maintainers []apitypes.SemanticMaintainerApproval
		want        string
	}{
		{"complete", nil, ""},
		{"complete", maintainers, semanticParkMaintainerAuth},
		{"incomplete", nil, semanticParkCoverageIncomplete},
		{"incomplete", maintainers, semanticParkCoverageIncomplete},
		{"uncertain", maintainers, semanticParkCoverageUncertain},
		{"unavailable", nil, semanticParkCoverageUnavailable},
		{"exhausted", maintainers, semanticParkCoverageUnavailable},
	} {
		got := expectedSemanticPark(&apitypes.SemanticGatePresentation{Coverage: apitypes.SemanticCoverageStatus{Outcome: tc.outcome}, Maintainers: tc.maintainers})
		if got != tc.want {
			t.Errorf("outcome=%s maintainers=%d: park=%q, want %q", tc.outcome, len(tc.maintainers), got, tc.want)
		}
	}
}
