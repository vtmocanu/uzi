package workersvc

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckAfterPreflightHookNilByDefault(t *testing.T) {
	if New(nil, nil, Params{}).planCrossCheckAfterPreflightHook != nil {
		t.Fatal("planCrossCheckAfterPreflightHook must be nil in a production-constructed service")
	}
}

func legacyTestCandidate(ms string) PlanCrossCheckCandidate {
	return PlanCrossCheckCandidate{
		PlanMd:               "plan",
		Milestones:           json.RawMessage(ms),
		RequiredCapabilities: []string{" b ", "a"},
		RequiredTools:        []string{"t"},
		SizeClass:            "s",
		BaseCommit:           "0123456789012345678901234567890123456789",
	}
}

func TestLegacyCheckedPlanMilestones(t *testing.T) {
	cand := legacyTestCandidate(` [ {"id":"m1","title":"One"} ] `)
	got, ok := legacyCheckedPlanMilestones(cand)
	if !ok || string(got) != `[{"id":"m1","title":"One"}]` {
		t.Fatalf("valid: ok=%v got=%s", ok, got)
	}
	if cand.RequiredCapabilities[0] != " b " {
		t.Fatalf("caller capability slice was rewritten: %q", cand.RequiredCapabilities)
	}
	for name, ms := range map[string]string{
		"blank": "", "null": "null", "object": `{"id":"m1"}`, "badshape": `[{"id":"m1"}]`, "garbage": `[`,
	} {
		if _, ok := legacyCheckedPlanMilestones(legacyTestCandidate(ms)); ok {
			t.Errorf("%s: expected no legacy identity", name)
		}
	}
}

func TestLegacyCrossCheckDigestAndPriorRules(t *testing.T) {
	ms := json.RawMessage(`[{"id":"m1","title":"One"}]`)
	cand := legacyTestCandidate(`[]`)
	canonical, err := cand.Digest()
	if err != nil {
		t.Fatal(err)
	}
	legacy := cand
	legacy.Milestones = ms
	want, _ := legacy.Digest()
	got := legacyCrossCheckDigest(cand, ms, true)
	if !bytes.Equal(got, want) || bytes.Equal(got, canonical) {
		t.Fatal("legacy digest must key on the received milestones")
	}
	if legacyCrossCheckDigest(cand, ms, false) != nil {
		t.Fatal("no legacy identity must yield nil")
	}
	cand.CandidateIdentityVersion = "v2"
	if legacyCrossCheckDigest(cand, ms, true) != nil {
		t.Fatal("bound semantic identity must yield nil")
	}

	prior := store.CrossCheck{LeadClaimGeneration: 3, CandidateDigest: want}
	if !priorMatches(prior, 3, canonical, want) || priorMatches(prior, 4, canonical, want) || priorMatches(prior, 3, canonical, nil) {
		t.Fatal("priorMatches: legacy digest accepted only with same generation and a legacy identity")
	}
	if !errors.Is(priorMismatchError(true, false), ErrCrossCheckCandidateInvalid) || !errors.Is(priorMismatchError(true, false), ErrCrossCheckRefused) || errors.Is(priorMismatchError(true, false), ErrCrossCheckInterrupted) {
		t.Fatal("malformed non-issue mismatch must preserve the candidate-invalid refusal")
	}
	if !errors.Is(priorMismatchError(true, true), ErrCrossCheckInterrupted) || !errors.Is(priorMismatchError(false, false), ErrCrossCheckInterrupted) {
		t.Fatal("valid non-issue and issue mismatches stay interrupted")
	}
}
