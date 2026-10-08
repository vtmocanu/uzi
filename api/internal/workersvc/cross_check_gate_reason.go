package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// validatePlanCrossCheckGateReason validates declarations only; SetRunAwaitingApproval
// derives the authoritative row outcome in its owned, generation-fenced UPDATE.
func (s *Service) validatePlanCrossCheckGateReason(ctx context.Context, q Store, worker store.Worker, lead store.Run, req StateRequest, fencedGate bool) error {
	reason := ""
	if req.PlanCrossCheckGateReason != nil {
		reason = *req.PlanCrossCheckGateReason
		if reason == "" {
			return ErrInvalidState
		}
	}
	switch reason {
	case "", "revise", "block", "malformed", "model_error", "model_timeout",
		"checker_unavailable", "confinement_failed", "timed_out", "superseded",
		"codex_lead_unsupported", "planning_diff_refused", "interrupted", "candidate_refused", "checker_failed", "revisions_exhausted":
	default:
		return ErrInvalidState
	}
	if req.PlanCrossCheckRefusal != "" && reason != "candidate_refused" && reason != "checker_failed" {
		return ErrInvalidState
	}
	if reason == "candidate_refused" || reason == "checker_failed" {
		// SQL may replace the declaration with its authoritative outcome. On the
		// locked, tracked gate path, let a current-ID replay reach classifyGateReport,
		// which still checks the approval-bearing payload before any report write.
		active := lead.AutoApprove && (lead.Status == "claimed" || lead.Status == "running")
		current := fencedGate && lead.ClaimGeneration > 0 && req.PresentationID != nil &&
			lead.GatePresentationID.Valid && uuid.UUID(lead.GatePresentationID.Bytes) == *req.PresentationID
		retained := lead.Status == "awaiting_approval" && !lead.AutoApprove &&
			(lead.PlanCrossCheckGateReason.String == reason || current)
		if !lead.PlanCrossCheckRequired || (!active && !retained) || !isCrossCheckLeadHarness(lead.Harness) ||
			req.ClaimGeneration == nil || *req.ClaimGeneration != lead.ClaimGeneration ||
			lead.ClaimReleasedAt.Valid || lead.WorkerID != pgconv.UUID(worker.ID) || lead.UserID != worker.UserID {
			return ErrInvalidState
		}
		switch reason {
		case "candidate_refused":
			switch req.PlanCrossCheckRefusal {
			case "candidate_too_large", "candidate_invalid", "envelope_too_large":
			default:
				return ErrInvalidState
			}
		case "checker_failed":
			if req.PlanCrossCheckRefusal != "submit_failed" {
				return ErrInvalidState
			}
		}
	}
	if req.PlanCrossCheckDiffRefusal != "" && reason != "planning_diff_refused" {
		return ErrInvalidState
	}
	if reason == "planning_diff_refused" {
		switch req.PlanCrossCheckDiffRefusal {
		case "base_unavailable", "diff_failed", "diff_too_large", "too_many_untracked", "secret_detected", "scan_failed", "unsupported_entry":
		default:
			return ErrInvalidState
		}
	}
	reader, ok := q.(interface {
		GetPlanCrossCheck(context.Context, uuid.UUID) (store.CrossCheck, error)
	})
	if !ok {
		return ErrInvalidState
	}
	_, err := reader.GetPlanCrossCheck(ctx, lead.ID)
	if err == nil {
		return nil
	} // Recorded non-pass outcomes win; an approval may retain a workflow refusal.
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	switch reason {
	case "":
		return nil
	case "interrupted":
		// A recovered ownership generation or retained lifecycle reason is
		// server evidence of interruption even before a candidate was stored.
		if lead.ClaimGeneration > 1 || lead.PlanCrossCheckGateReason.String == "interrupted" {
			return nil
		}
	case "codex_lead_unsupported":
		if lead.Harness == string(HarnessCodex) {
			return nil
		}
	case "checker_unavailable":
		if worker.IsolatedLane {
			return nil
		}
		resolver, ok := q.(harnessResolverStore)
		if !ok {
			return ErrInvalidState
		}
		// The checker is the lead's OPPOSITE family, so an unusable credential of that
		// family (never the lead's own) is what attests the checker unavailable.
		harness, known := oppositeHarness(lead.Harness)
		if !known {
			return ErrInvalidState
		}
		_, err := s.resolveRunHarnessQ(ctx, lead.UserID, &harness, resolver)
		if errors.Is(err, ErrNoCredentialForHarness) || errors.Is(err, ErrHarnessCredentialDisabled) {
			return nil
		}
		if err != nil {
			return err
		}
	case "candidate_refused", "checker_failed":
		// GetPlanCrossCheck proved absence under the owning lead transaction's
		// lock. This attestation only parks the lead; it grants no plan authority.
		return nil
	case "planning_diff_refused":
		// This bounded declaration attests capture failure; it never authorizes a pass.
		if !isCrossCheckLeadHarness(lead.Harness) || worker.IsolatedLane {
			return ErrInvalidState
		}
		switch req.PlanCrossCheckDiffRefusal {
		case "base_unavailable", "diff_failed", "diff_too_large", "too_many_untracked", "secret_detected", "scan_failed", "unsupported_entry":
			return nil
		}
	}
	return ErrInvalidState
}
