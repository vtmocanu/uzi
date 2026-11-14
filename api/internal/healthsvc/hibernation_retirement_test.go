package healthsvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The fleet.hibernation_retirement dry run (PRD #4208 M1): severity, summary and the exact
// evidence text. The evaluator itself is covered by the workersvc LiveDB tests; these drive the
// check through the Config seam with canned reports.

const wantConditional = "would retire only after M2 verifies: actual PV reclaim policy; no intervening pod between observations (sampled only in M1)"

func retirementSvc(rep workersvc.RetirementReport, err error) *Service {
	return New(Config{Store: &fakeStore{}, Settings: &fakeSettings{}, Now: func() time.Time { return fixedNow },
		HibernationRetirement: func(context.Context) (workersvc.RetirementReport, error) { return rep, err }})
}

func conditionalVerdict(id uuid.UUID, policies ...workersvc.ClaimReclaimPolicy) workersvc.RetirementVerdict {
	v := workersvc.RetirementVerdict{WorkerID: id, Outcome: workersvc.RetirementConditionalRetire, PastPeriod: true,
		Unverified:    []string{workersvc.RetirementUnverifiedPVReclaimPolicy, workersvc.RetirementUnverifiedInterveningPodExcluded},
		ClassPolicies: policies}
	for _, p := range policies {
		v.Retain = v.Retain || p.Policy == "Retain"
		v.Unknown = v.Unknown || p.Policy == "unknown"
	}
	return v
}

func blockedVerdict(id uuid.UUID, code, source string) workersvc.RetirementVerdict {
	return workersvc.RetirementVerdict{WorkerID: id, Outcome: workersvc.RetirementBlocked, PastPeriod: true,
		Blockers: []workersvc.RetirementBlocker{{Code: code, Source: source}}, BlockerCount: 1}
}

func TestHibernationRetirementCheck(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	del := workersvc.ClaimReclaimPolicy{Role: "data", Policy: "Delete"}
	retain := workersvc.ClaimReclaimPolicy{Role: "dind_data", Policy: "Retain"}
	notYet := workersvc.RetirementVerdict{WorkerID: b, Outcome: workersvc.RetirementNotYetEligible, Remaining: 3*24*time.Hour + 5*time.Hour,
		Blockers: []workersvc.RetirementBlocker{{Code: "no_receipt", Source: "run-x"}}, BlockerCount: 1}
	tests := []struct {
		name         string
		rep          workersvc.RetirementReport
		err          error
		wantSev      string
		wantSummary  []string
		wantEvidence map[string]string
	}{
		{"na when nothing is hibernated", workersvc.RetirementReport{}, nil, sevNA, []string{"Dry run: no volumes are deleted"}, nil},
		{"ok when only conditional workers exist", workersvc.RetirementReport{Total: 1, Evaluated: 1, Verdicts: []workersvc.RetirementVerdict{conditionalVerdict(a, del)}}, nil, sevOK,
			[]string{"Dry run: no volumes are deleted"}, map[string]string{a.String()[:8]: wantConditional + "; class policy Delete"}},
		{"retain is flagged", workersvc.RetirementReport{Total: 1, Evaluated: 1, Verdicts: []workersvc.RetirementVerdict{conditionalVerdict(a, retain, retain)}}, nil, sevOK, nil,
			map[string]string{a.String()[:8]: wantConditional + "; class policy Retain (flagged)"}},
		{"mixed policies list each claim", workersvc.RetirementReport{Total: 1, Evaluated: 1, Verdicts: []workersvc.RetirementVerdict{conditionalVerdict(a, del, retain)}}, nil, sevOK, nil,
			map[string]string{a.String()[:8]: wantConditional + "; class policy data=Delete, dind_data=Retain (flagged)"}},
		{"warn when a worker past the period is blocked", workersvc.RetirementReport{Total: 2, Evaluated: 2, Verdicts: []workersvc.RetirementVerdict{
			conditionalVerdict(a, del), blockedVerdict(c, "no_receipt", "run-1")}}, nil, sevWarn, []string{"Dry run: no volumes are deleted"},
			map[string]string{c.String()[:8]: "blocked by no_receipt (run-1)"}},
		{"a not yet eligible blocked worker does not warn", workersvc.RetirementReport{Total: 1, Evaluated: 1, Verdicts: []workersvc.RetirementVerdict{notYet}}, nil, sevOK, nil,
			map[string]string{b.String()[:8]: "not yet eligible: 3d5h left; blocked by no_receipt (run-x)"}},
		{"cancelled", workersvc.RetirementReport{Total: 1, Evaluated: 1, Verdicts: []workersvc.RetirementVerdict{{WorkerID: a, Outcome: workersvc.RetirementCancelled}}}, nil, sevOK, nil,
			map[string]string{a.String()[:8]: "cancelled: wake requested"}},
		{"warn when more workers exist than are evaluated", workersvc.RetirementReport{Total: 51, Evaluated: 50, Verdicts: []workersvc.RetirementVerdict{conditionalVerdict(a, del)}}, nil, sevWarn,
			[]string{"Dry run: no volumes are deleted", "evaluated 50 of 51 (oldest first)"}, nil},
		{"an unavailable hibernation service is na", workersvc.RetirementReport{}, workersvc.ErrHibernationUnavailable, sevNA, []string{"Dry run: no volumes are deleted"}, nil},
		{"evaluator error degrades to unknown", workersvc.RetirementReport{}, errors.New("boom"), sevUnknown, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := retirementSvc(tc.rep, tc.err).checkFleetHibernationRetirement(context.Background())
			if got.Severity != tc.wantSev {
				t.Fatalf("severity = %q (%s), want %q", got.Severity, got.Summary, tc.wantSev)
			}
			for _, s := range tc.wantSummary {
				if !strings.Contains(got.Summary, s) {
					t.Fatalf("summary %q lacks %q", got.Summary, s)
				}
			}
			for label, value := range tc.wantEvidence {
				found := false
				for _, e := range got.Evidence {
					if e.Label == label {
						found = true
						if e.Value != value {
							t.Fatalf("evidence %s = %q, want %q", label, e.Value, value)
						}
					}
				}
				if !found {
					t.Fatalf("no evidence row %s in %+v", label, got.Evidence)
				}
			}
			if got.Scope != "instance" || got.Group != groupWorkers || got.Doc == nil || *got.Doc != "hosted-workers" {
				t.Fatalf("meta = %q/%q/%v", got.Scope, got.Group, got.Doc)
			}
		})
	}
}

func TestHibernationRetirementEvidenceBoundsAndSanitizes(t *testing.T) {
	var verdicts []workersvc.RetirementVerdict
	for range 30 {
		verdicts = append(verdicts, conditionalVerdict(uuid.New()))
	}
	hostile := blockedVerdict(uuid.New(), "protected_source", "bare_repo:/data/"+strings.Repeat("x", 500)+"\x1b[31m\nred")
	verdicts = append(verdicts, hostile)
	got := retirementSvc(workersvc.RetirementReport{Total: 31, Evaluated: 31, Verdicts: verdicts}, nil).checkFleetHibernationRetirement(context.Background())
	if len(got.Evidence) != maxQuarantineEvidence {
		t.Fatalf("evidence rows = %d, want %d", len(got.Evidence), maxQuarantineEvidence)
	}
	// Blocked workers sort first, so the hostile one is shown, bounded and stripped.
	first := got.Evidence[0].Value
	if !strings.HasPrefix(first, "blocked by protected_source (") || strings.ContainsAny(first, "\x1b\n") || len(first) > 200 {
		t.Fatalf("hostile source not bounded: %q", first)
	}
	if got.Severity != sevWarn {
		t.Fatalf("severity = %q, want warn", got.Severity)
	}
}

func TestHibernationRetirementUnwiredIsNA(t *testing.T) {
	got := newSvc(&fakeStore{}, &fakeSettings{}).checkFleetHibernationRetirement(context.Background())
	if got.Severity != sevNA || !strings.Contains(got.Summary, "Dry run: no volumes are deleted") {
		t.Fatalf("unwired = %q %q", got.Severity, got.Summary)
	}
}
