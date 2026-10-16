package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPolicyHoldDoesNotAdvertiseIncompleteOwnerChoices(t *testing.T) {
	for _, reason := range []string{"provider_policy_refusal", "budget_exhausted"} {
		t.Run(reason, func(t *testing.T) {
			r := store.Run{Status: "paused", Kind: "issue", Harness: "codex", PolicyRefusalCount: 1,
				HoldReason:          pgtype.Text{String: reason, Valid: true},
				MilestonesFrozen:    json.RawMessage(`[{"id":"M1","title":"first"}]`),
				MilestonesCompleted: json.RawMessage(`["M1"]`)}
			dto := runToDTO(r, "normal", time.Hour, 3600, 3, time.Now(), 3)
			if dto.CanStopAtWall || dto.CanStopAtProviderHold || dto.PolicyRefusal == nil {
				t.Fatalf("unfinished policy owner choice advertised: %+v", dto.PolicyRefusal)
			}
		})
	}
}
