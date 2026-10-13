package workersvc

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// rollWaitReason requires every suitable candidate to be unavailable through a roll.
// The context comes from one eligible worker; no other owner's canary is consulted.
func rollWaitReason(now time.Time, counts store.CountOnlineWorkersClaimableForRunRow) string {
	if counts.DiagnosticSuitable == 0 || counts.DiagnosticSuitable != counts.DiagnosticRolling {
		return ""
	}
	// An entirely healthy active drain is still the existing upgrade wait:
	// queue/capacity policy confirms its exact stored reason and finite drain.
	// Held/evidence waits and any actual replacement gap do not get that exemption.
	if counts.DiagnosticDrainingOnly == counts.DiagnosticSuitable {
		return ""
	}
	var context struct {
		Pacing         json.RawMessage `json:"pacing"`
		ObservedAt     time.Time       `json:"observed_at"`
		BlockingReason string          `json:"blocking_reason"`
	}
	if len(counts.DiagnosticRollContext) > 8192 || json.Unmarshal(counts.DiagnosticRollContext, &context) != nil {
		return ""
	}
	p := ParseFreshPacingReport(context.Pacing, pgtype.Timestamptz{Time: context.ObservedAt, Valid: true}, now)
	if p == nil {
		return ""
	}
	reason := termsafe.SanitizeBounded(termsafe.CellText(context.BlockingReason), 192)
	if p.BlockingReason != nil && *p.BlockingReason != "" {
		reason = *p.BlockingReason
	}
	if reason == "" {
		reason = "waiting for replacement registration and heartbeat"
	}
	return fmt.Sprintf("waiting for worker roll (%s) to %s: %s", p.State, p.TargetImage, reason)
}

// DrainOnlyEligibleForHealth retains the legacy eligibility predicate when no
// fresh pacing is available. Once pacing identifies a roll, every suitable
// provider must be a healthy active drainer before the upgrade exemption applies.
// Callers independently validate the stored reason and finite overlap timestamps.
func DrainOnlyEligibleForHealth(counts store.CountOnlineWorkersClaimableForRunRow) bool {
	return counts.DrainingEligible > 0 && counts.NonDrainingEligible == 0 && counts.SuitableOwnDraining == 0 &&
		(counts.DiagnosticRolling == 0 || counts.DiagnosticDrainingOnly == counts.DiagnosticSuitable)
}
