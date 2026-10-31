package handler

import (
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"strings"
	"testing"
)

func TestParseWorkerPolicyTelemetry(t *testing.T) {
	run, id, worker := uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name, id, gen string
		valid         bool
	}{
		{"valid", fmt.Sprintf("%q", id.String()), "3", true},
		{"missing", "", "", false},
		{"wrong id type", "{}", "3", false},
		{"nil id", fmt.Sprintf("%q", uuid.Nil.String()), "3", false},
		{"uppercase id", fmt.Sprintf("%q", strings.ToUpper(id.String())), "3", false},
		{"source string", fmt.Sprintf("%q", id.String()), "\"3\"", false},
		{"zero source", fmt.Sprintf("%q", id.String()), "0", false},
		{"unsafe source", fmt.Sprintf("%q", id.String()), "9007199254740992", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := ""
			if tc.id != "" {
				extra = ",\"policy_pending_outcome_id\":" + tc.id + ",\"policy_pending_source_generation\":" + tc.gen
			}
			raw := fmt.Sprintf("[{\"run_id\":%q,\"pending_messages\":2,\"pending_terminal\":1,\"stale_retired\":0,\"since\":1%s}]", run.String(), extra)
			got := parseWorkerOutbox(json.RawMessage(raw), worker)
			if len(got) != 1 || got[0].PendingMessages != 2 || got[0].PendingTerminal != 1 {
				t.Fatal("optional telemetry damaged depth")
			}
			if tc.valid {
				if got[0].PolicyPendingOutcomeID != id || got[0].PolicyPendingSourceGeneration != 3 {
					t.Fatal("lost valid original identity")
				}
			} else if got[0].PolicyPendingOutcomeID != uuid.Nil || got[0].PolicyPendingSourceGeneration != 0 {
				t.Fatal("invalid facts survived")
			}
		})
	}
}

func TestPolicyHoldDTOCaptureIsCurrentOnly(t *testing.T) {
	head := strings.Repeat("a", 40)
	r := store.Run{Status: "paused", HoldReason: pgconv.Text("codex_policy_hold"), PolicyOutcomeID: pgconv.UUID(uuid.New()), HoldCapturedHead: pgconv.Text(head)}
	got := policyHoldDTO(r)
	if got == nil || got.CapturedHead == nil || *got.CapturedHead != head {
		t.Fatal("missing current capture")
	}
	r.HoldReason = pgconv.Text("completion_blocked")
	if got := policyHoldDTO(r); got.CapturedHead != nil {
		t.Fatal("ordinary later capture was attributed to policy history")
	}
	r.HoldReason = pgconv.Text("codex_policy_hold")
	r.PolicyResolvedAt.Valid = true
	if got := policyHoldDTO(r); got.CapturedHead != nil {
		t.Fatal("resolved history advertised a current capture")
	}
}
