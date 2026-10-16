package handler

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPolicyOwnerChoiceAvailability(t *testing.T) {
	for _, reason := range []string{"provider_policy_refusal", "budget_exhausted", "completion_blocked"} {
		t.Run(reason, func(t *testing.T) {
			r := store.Run{Status: "paused", Kind: "issue", Harness: "codex", PolicyRefusalCount: 1,
				HoldReason:          pgtype.Text{String: reason, Valid: true},
				MilestonesFrozen:    json.RawMessage(`[{"id":"M1","title":"first"}]`),
				MilestonesCompleted: json.RawMessage(`["M1"]`)}
			if reason == "completion_blocked" {
				r.PolicyLastOutcomeID = pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
				dto := runToDTO(r, "normal", time.Hour, 3600, 3, time.Now(), 3)
				if dto.PolicyRefusal == nil || dto.PolicyRefusal.CanResume || dto.PolicyRefusal.ActionIfMatch == "" {
					t.Fatal("completion decision must retain the presented policy token")
				}
				return
			}
			dto := runToDTO(r, "normal", time.Hour, 3600, 3, time.Now(), 3)
			r.StartedAt = pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Hour), Valid: true}
			r.StatusSince = pgtype.Timestamptz{Time: time.Now(), Valid: true}
			spent := runToDTO(r, "normal", time.Hour, 3600, 3, time.Now(), 3)
			want := reason != "budget_exhausted"
			if spent.PolicyRefusal.CanResume != want || spent.CanStopAtProviderHold != want {
				t.Fatalf("spent %s availability: %+v", reason, spent.PolicyRefusal)
			}
			r.ClaimGeneration = math.MaxInt64
			overflow := runToDTO(r, "normal", time.Hour, 3600, 3, time.Now(), 3)
			if overflow.PolicyRefusal.CanResume || overflow.CanStopAtProviderHold {
				t.Fatal("unadvanceable claim advertised resume or Stop")
			}
			if dto.CanStopAtWall || !dto.CanStopAtProviderHold || dto.PolicyRefusal == nil || !dto.PolicyRefusal.CanResume {
				t.Fatalf("policy owner choices missing: %+v", dto.PolicyRefusal)
			}
		})
	}
}
