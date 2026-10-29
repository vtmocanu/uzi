package workersvc

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func policyHoldFixture() PolicyHoldRequest {
	tag, phase, correlation := "cyberPolicy", "planning", "pr-00000000000000000000000000000000-1"
	return PolicyHoldRequest{OutcomeID: uuid.NewString(), SourceGeneration: 1, ClaimGeneration: 1,
		Detail: "refused", ContinuationMode: "same_conversation", PolicyTag: &tag, Phase: &phase, CorrelationID: &correlation}
}

func TestPolicyHoldValidation(t *testing.T) {
	for _, tag := range []string{"cyberPolicy", "misalignmentPolicyViolation"} {
		for _, phase := range []string{"planning", "implementation"} {
			r := policyHoldFixture()
			r.PolicyTag = &tag
			r.Phase = &phase
			if err := r.Validate(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, change := range map[string]func(*PolicyHoldRequest){
		"zero uuid":                    func(r *PolicyHoldRequest) { r.OutcomeID = uuid.Nil.String() },
		"uppercase uuid":               func(r *PolicyHoldRequest) { r.OutcomeID = "ABCDEFAB-0000-4000-8000-000000000001" },
		"noncanonical uuid":            func(r *PolicyHoldRequest) { r.OutcomeID = strings.ReplaceAll(r.OutcomeID, "-", "") },
		"zero source":                  func(r *PolicyHoldRequest) { r.SourceGeneration = 0 },
		"future source":                func(r *PolicyHoldRequest) { r.SourceGeneration = 2 },
		"unsafe generation":            func(r *PolicyHoldRequest) { r.ClaimGeneration = 9007199254740992 },
		"unknown detail":               func(r *PolicyHoldRequest) { r.Detail = "failed" },
		"unknown mode":                 func(r *PolicyHoldRequest) { r.ContinuationMode = "fresh" },
		"missing tag":                  func(r *PolicyHoldRequest) { r.PolicyTag = nil },
		"missing phase":                func(r *PolicyHoldRequest) { r.Phase = nil },
		"missing correlation":          func(r *PolicyHoldRequest) { r.CorrelationID = nil },
		"provider prose":               func(r *PolicyHoldRequest) { s := "refused for policy"; r.PolicyTag = &s },
		"child phase":                  func(r *PolicyHoldRequest) { s := "review"; r.Phase = &s },
		"correlation trailing newline": func(r *PolicyHoldRequest) { s := *r.CorrelationID + "\n"; r.CorrelationID = &s },
		"correlation unsafe counter": func(r *PolicyHoldRequest) {
			s := "pr-00000000000000000000000000000000-9007199254740992"
			r.CorrelationID = &s
		},
		"correlation leading zero": func(r *PolicyHoldRequest) { s := "pr-00000000000000000000000000000000-01"; r.CorrelationID = &s },
		"session nul":              func(r *PolicyHoldRequest) { s := "session\x00"; r.SessionID = &s },
		"session oversized":        func(r *PolicyHoldRequest) { s := strings.Repeat("a", 1025); r.SessionID = &s },
		"published without head":   func(r *PolicyHoldRequest) { r.Published = true },
		"latest without head":      func(r *PolicyHoldRequest) { v := true; r.CheckpointContainsLatest = &v },
		"invalid head":             func(r *PolicyHoldRequest) { s := "HEAD"; r.Head = &s },
	} {
		t.Run(name, func(t *testing.T) {
			r := policyHoldFixture()
			change(&r)
			if r.Validate() == nil {
				t.Fatal("accepted invalid policy hold")
			}
		})
	}
	r := policyHoldFixture()
	r.Detail = "resume_rejected"
	r.PolicyTag = nil
	r.Phase = nil
	r.CorrelationID = nil
	r.SourceGeneration = 1
	r.ClaimGeneration = 9007199254740991
	r.ContinuationMode = "fresh_from_saved_work"
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	s := "pr-00000000000000000000000000000000-9007199254740991"
	r.CorrelationID = &s
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}
