package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The embedded Store panics on every unexpected operation, including every write.
type semanticNoTxReadStore struct {
	Store
	run store.Run
}

func (s *semanticNoTxReadStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return s.run, nil
}

func TestSemanticCompletionNoTx(t *testing.T) {
	for _, contaminated := range []bool{false, true} {
		for _, operation := range []string{"permit", "attempt", "hold", "running", "awaiting_approval", "awaiting_input", "paused", "failed", "pause_failed"} {
			t.Run(operation+map[bool]string{false: "/semantic", true: "/contaminated"}[contaminated], func(t *testing.T) {
				run := semanticCompletionFixture(t, false)
				if contaminated {
					run.CompletionContract = contractJSON(t, "m1")
					c, _ := parseCompletionContract(run.CompletionContract)
					c.SourceOutcome = "complete"
					run.CompletionContract = semanticJSON(t, c)
				}
				svc := New(&semanticNoTxReadStore{run: run}, newBox(t), testParams())
				// A panic names a forbidden call and must fail this regression.
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("unlocked mutation reached store: %v", p)
					}
				}()
				var err error
				switch operation {
				case "permit":
					_, err = svc.RequestCompletionPermit(context.Background(), worker(), run.ID, CompletionPermitRequest{ContractRevision: 1, Branch: "agent/test", Head: "head"})
				case "attempt":
					_, err = svc.RecordCompletionAttempt(context.Background(), worker(), run.ID, CompletionAttemptRequest{})
				case "hold":
					_, _, err = svc.SetRunCompletionHold(context.Background(), worker(), run.ID, "head", nil)
				default:
					_, _, _, err = svc.SetStateReport(context.Background(), worker(), run.ID, StateRequest{State: operation})
				}
				if !errors.Is(err, ErrSemanticCompletionAuthority) {
					t.Fatalf("got %v, want authority unavailable", err)
				}
			})
		}
	}
}

func TestSemanticCompletionCredentialStateNoTx(t *testing.T) {
	for _, state := range []string{"credential_switch", "credential_switch_failed"} {
		for _, stamp := range []bool{false, true} {
			t.Run(state+map[bool]string{false: "/missing", true: "/stamped"}[stamp], func(t *testing.T) {
				run := semanticCompletionFixture(t, false)
				run.ClaimGeneration = 1
				req := StateRequest{State: state}
				if stamp {
					req.ClaimGeneration = &run.ClaimGeneration
				}
				svc := New(&semanticNoTxReadStore{run: run}, newBox(t), testParams())
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("unlocked credential mutation reached store: %v", p)
					}
				}()
				_, _, _, err := svc.SetStateReport(context.Background(), worker(), run.ID, req)
				want := ErrInvalidState
				if stamp {
					want = ErrSemanticCompletionAuthority
				}
				if !errors.Is(err, want) {
					t.Fatalf("got %v, want %v", err, want)
				}
			})
		}
	}
}

func TestSemanticCompletionMalformedNoTx(t *testing.T) {
	for _, raw := range []string{
		`{"profile":"semantic","revision":"bad"}`,
		`{"profile":1,"semantic_approval":null}`,
		`{"profile":"structural","revision":"bad","sources":[]}`,
		`{"profile":"structural","criteria":[{"kind":42}]}`,
		`{"profile":"structural","criteria":[17,{"maintainer":false}]}`,
		`{"profile":"structural","coverage":false}`,
		`{"Profile":"semantic","revision":"bad"}`,
		`{"profile":"structural","revision":"bad","\u017fources":[{}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			run := semanticCompletionFixture(t, false)
			run.CompletionContract = json.RawMessage(raw)
			svc := New(&semanticNoTxReadStore{run: run}, newBox(t), testParams())
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("unlocked mutation reached store: %v", p)
				}
			}()
			_, _, _, err := svc.SetStateReport(context.Background(), worker(), run.ID, StateRequest{State: "paused"})
			if !errors.Is(err, ErrSemanticCompletionAuthority) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestSemanticCompletionStoredReaderStrictness(t *testing.T) {
	raw := string(semanticCompletionFixture(t, false).CompletionContract)
	for name, corrupt := range map[string]string{
		"unknown":          strings.Replace(raw, "{", "{\"extra\":1,", 1),
		"alias":            strings.Replace(raw, "\"profile\"", "\"Profile\"", 1),
		"duplicate":        strings.Replace(raw, "{", "{\"profile\":\"semantic\",", 1),
		"trailing":         raw + "{}",
		"surrogate":        strings.Replace(raw, "\"audit\":null", "\"audit\":\"\\ud800\"", 1),
		"utf8":             strings.Replace(raw, "\"audit\":null", "\"audit\":\""+string([]byte{0xff})+"\"", 1),
		"opaque_duplicate": strings.Replace(raw, "\"audit\":null", "\"audit\":{\"a\":1,\"a\":2}", 1),
		"opaque_alias":     strings.Replace(raw, "\"audit\":null", "\"audit\":{\"A\":1}", 1),
		"nested_alias":     strings.Replace(raw, "\"reason\"", "\"Reason\"", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if corrupt == raw {
				t.Fatal("fixture mutation did not apply")
			}
			var original completionContract
			if strictSemanticJSON([]byte(corrupt), &original) == nil {
				t.Fatal("invalid strictness fixture")
			}
			if _, err := parseCompletionContract([]byte(corrupt)); err == nil {
				t.Fatal("stored reader accepted corrupt JSON")
			}
		})
	}
	var original completionContract
	if err := strictSemanticJSON([]byte(raw), &original); err != nil {
		t.Fatal(err)
	}
	decoded, err := parseCompletionContract([]byte(raw))
	if err != nil || !sameSemanticJSON(original, decoded) {
		t.Fatalf("valid semantic read changed: %v", err)
	}
	// Paired surrogates and permitted prose controls remain data.
	valid := strings.Replace(raw, "\"audit\":null", "\"audit\":{\"a\":\"\\ud83d\\ude00\\n\\u202e\"}", 1)
	if _, err := parseCompletionContract([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	structural := []byte("{\"profile\":\"structural\",\"revision\":1,\"unknown\":true,\"criteria\":[]}")
	if _, err := parseCompletionContract(structural); err != nil {
		t.Fatalf("legacy fallback: %v", err)
	}
}

func TestSemanticCompletionStoredReaderCustomDecoderBoundaries(t *testing.T) {
	contract, err := parseCompletionContract(semanticCompletionFixture(t, false).CompletionContract)
	if err != nil {
		t.Fatal(err)
	}
	// Atomicity lives in the frozen presentation's coverage report.
	contract.SemanticPresentation.Coverage.Report = &apitypes.SemanticCoverageReport{
		Criteria: []apitypes.SemanticCoverageAtomicity{{
			CriterionID: contract.Criteria[0].ID, Atomic: true, Explanation: "one behavior",
		}},
	}
	raw := string(semanticJSON(t, contract))
	for _, tc := range []struct {
		name, old, replacement string
		valid                  bool
	}{
		{"citation duplicate", `"start":0`, `"start":0,"start":0`, false},
		{"citation alias", `"start":0`, `"Start":0`, false},
		{"citation missing start", `"start":0,`, "", false},
		{"citation missing end", `,"end":6`, "", false},
		{"citation null start", `"start":0`, `"start":null`, false},
		{"citation null end", `"end":6`, `"end":null`, false},
		{"evidence duplicate description", `"description":"test output"`, `"description":"test output","description":"test output"`, false},
		{"evidence alias description", `"description":"test output"`, `"Description":"test output"`, false},
		{"evidence duplicate path", `"path":"api/test.go"`, `"path":"api/test.go","path":"api/test.go"`, false},
		{"evidence alias path", `"path":"api/test.go"`, `"Path":"api/test.go"`, false},
		{"evidence null path", `"path":"api/test.go"`, `"path":null`, false},
		{"atomicity missing", `"atomic":true,`, "", false},
		{"atomicity null", `"atomic":true`, `"atomic":null`, false},
		{"atomicity duplicate", `"atomic":true`, `"atomic":true,"atomic":true`, false},
		{"atomicity alias", `"atomic":true`, `"Atomic":true`, false},
		{"evidence absent path", `,"path":"api/test.go"`, "", true},
		{"evidence empty path", `"path":"api/test.go"`, `"path":""`, true},
		{"atomicity false", `"atomic":true`, `"atomic":false`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := strings.Replace(raw, tc.old, tc.replacement, 1)
			if mutated == raw {
				t.Fatal("fixture mutation did not apply")
			}
			var original completionContract
			strictErr := strictSemanticJSON([]byte(mutated), &original)
			decoded, readErr := parseCompletionContract([]byte(mutated))
			if !tc.valid {
				if strictErr == nil {
					t.Fatal("reference strictSemanticJSON accepted invalid fixture")
				}
				if readErr == nil {
					t.Fatal("stored reader accepted corrupt JSON")
				}
				return
			}
			if strictErr != nil || readErr != nil || !sameSemanticJSON(original, decoded) {
				t.Fatalf("valid custom decoder control changed: strict=%v stored=%v", strictErr, readErr)
			}
		})
	}
	var original completionContract
	if err := strictSemanticJSON([]byte(raw), &original); err != nil {
		t.Fatal(err)
	}
	if decoded, err := parseCompletionContract([]byte(raw)); err != nil || !sameSemanticJSON(original, decoded) {
		t.Fatalf("valid report control changed: %v", err)
	}
}

func TestSemanticCompletionContaminatedDecisionRefused(t *testing.T) {
	run := semanticCompletionFixture(t, false)
	c, err := parseCompletionContract(run.CompletionContract)
	if err != nil {
		t.Fatal(err)
	}
	c.Profile = contractProfileStructural
	run.CompletionContract = semanticJSON(t, c)
	run.MilestonesFrozen = frozenJSON(t, "m1", "m2")
	ms, _ := DecodeMilestones(run.MilestonesFrozen)
	control := run
	control.CompletionContract = contractJSON(t, "m1", "m2")
	for _, dec := range []CompletionDecisionInput{
		{Decision: "partial", Keep: []string{"m1"}, Reason: "later", ContractRevision: 1},
		{Decision: "accept", Criteria: []string{c.Criteria[0].ID}, Reason: "accept", ContractRevision: 1},
	} {
		controlDecision := dec
		if dec.Decision == "accept" {
			controlDecision.Criteria = []string{"m1.c1"}
		}
		if err := validateDecision(control, controlDecision, ms); err != nil {
			t.Fatalf("structural control %s: %v", dec.Decision, err)
		}
		if err := validateDecision(run, dec, ms); !errors.Is(err, ErrCompletionRevisionConflict) {
			t.Errorf("%s admitted contaminated authority: %v", dec.Decision, err)
		}
	}
	accept := CompletionDecisionInput{Decision: "accept", Criteria: []string{c.Criteria[0].ID}, Reason: "accept", ContractRevision: 1}
	priorBad, err := buildRevisedContract(run.CompletionContract, accept, ms, 2)
	if err != nil {
		t.Fatal(err)
	}
	run.CompletionContract, run.ContractRevision.Int32 = priorBad, 2
	if decisionAlreadyEncoded(run, accept, 2) {
		t.Error("replay acknowledged contaminated authority")
	}
}

func TestSemanticCompletionSourceOutcomeContamination(t *testing.T) {
	run := interlockedRun(worker())
	run.CompletionContract = contractJSON(t, "m1")
	run.MilestonesCompleted = idsJSON(t, "m1")
	before := string(run.CompletionContract)
	if pending, err := CompletionPending(run); err != nil || pending != nil {
		t.Fatalf("structural: %v %v", pending, err)
	}
	if _, valid := computeUnmetCriteria(run); !valid {
		t.Fatal("structural contract invalid")
	}
	c, _ := parseCompletionContract(run.CompletionContract)
	if string(semanticJSON(t, c)) != before {
		t.Fatal("structural serialization changed")
	}
	c.SourceOutcome = "complete"
	run.CompletionContract = semanticJSON(t, c)
	if _, err := CompletionPending(run); !errors.Is(err, ErrSemanticCompletionAuthority) {
		t.Errorf("contamination projected: %v", err)
	}
	if _, valid := computeUnmetCriteria(run); valid {
		t.Fatal("contamination considered verifiable")
	}
}

func semanticCompletionTwoMaintainers(t *testing.T) store.Run {
	t.Helper()
	run := semanticCompletionFixture(t, false)
	c, _ := parseCompletionContract(run.CompletionContract)
	second := c.SemanticPresentation.Candidate.Criteria[0]
	second.ID = uuid.NewString()
	copyMaintainer := *second.Maintainer
	copyMaintainer.Conditions = []string{"second deployment available"}
	second.Maintainer = &copyMaintainer
	c.SemanticPresentation.Candidate.Criteria = append(c.SemanticPresentation.Candidate.Criteria, second)
	p := semanticGateProjection(&semanticCoverageTx{candidate: c.SemanticPresentation.Candidate}, run)
	a := semanticApprovalFor(run, p)
	var err error
	run.CompletionContract, err = semanticGateContract(&p, &a)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestSemanticCompletionSelectedAuthorizationReplay(t *testing.T) {
	run := semanticCompletionTwoMaintainers(t)
	prior, _ := parseCompletionContract(run.CompletionContract)
	all := semanticMaintainers(prior)
	dec := CompletionDecisionInput{Decision: "authorize_maintainer", Maintainers: all[:1], Reason: "renew selected", ContractRevision: 1}
	ms, _ := DecodeMilestones(run.MilestonesFrozen)
	if err := validateDecision(run, dec, ms); err != nil {
		t.Fatal(err)
	}
	next, err := buildRevisedContract(run.CompletionContract, dec, ms, 2)
	if err != nil {
		t.Fatal(err)
	}
	run.CompletionContract = next
	run.ContractRevision.Int32 = 2
	current, _ := parseCompletionContract(next)
	for _, auth := range current.MaintainerAuthorizations {
		want := 1
		if auth.ID == all[0].ID {
			want = 2
		}
		if auth.Revision != want {
			t.Fatalf("authorization %s revision=%d want=%d", auth.ID, auth.Revision, want)
		}
	}
	if !decisionAlreadyEncoded(run, dec, 2) {
		t.Fatal("exact selected replay lost")
	}
	broader := dec
	broader.Maintainers = all
	if decisionAlreadyEncoded(run, broader, 2) {
		t.Fatal("all authorizations falsely replayed subset")
	}
	other := dec
	other.Maintainers = all[1:]
	if decisionAlreadyEncoded(run, other, 2) {
		t.Fatal("different subset falsely replayed")
	}
	if !decisionAlreadyEncoded(run, dec, 2) || string(run.CompletionContract) != string(next) {
		t.Fatal("replay changed revision")
	}
}
