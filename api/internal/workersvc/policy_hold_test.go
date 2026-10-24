package workersvc

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPolicyCorrelationContract(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/policy-hold/correlation.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string
		Value string
		Valid bool
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if got := ValidPolicyCorrelation(tc.Value); got != tc.Valid {
				t.Fatalf("correlation %q valid=%v want %v", tc.Value, got, tc.Valid)
			}
		})
	}
}

func TestPolicyRevisionResumeWithoutSession(t *testing.T) {
	r := store.Run{PlanMd: pgtype.Text{String: "original plan", Valid: true}, GateRevision: 7, PolicyRefusalCount: 1}
	if got := resumePhaseFor(r, false); got != "awaiting_approval" {
		t.Fatalf("verified unapproved gate was lost: phase=%q", got)
	}
	r.PolicyRefusalCount = 0
	if got := resumePhaseFor(r, false); got != "" {
		t.Fatalf("ordinary sessionless plan changed: %q", got)
	}
}

func TestPolicyHoldValidation(t *testing.T) {
	req := PolicyHoldRequest{OutcomeID: "00000000-0000-4000-8000-000000000001", SourceGeneration: 1, ClaimGeneration: 1, Detail: "resume_rejected"}
	if err := req.validate(); err != nil {
		t.Fatal(err)
	}
	req.Detail = "refused"
	if req.validate() == nil {
		t.Fatal("metadata-free refusal accepted")
	}
	tag, phase, correlation := "cyberPolicy", "planning", "pr-00000000000000000000000000000000-1"
	req.Tag, req.Phase, req.CorrelationID = &tag, &phase, &correlation
	if err := req.validate(); err != nil {
		t.Fatal(err)
	}
	phase = "provider text"
	if req.validate() == nil {
		t.Fatal("open metadata accepted")
	}
}
