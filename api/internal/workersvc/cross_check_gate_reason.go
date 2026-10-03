package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// validatePlanCrossCheckGateReason validates declarations only; SetRunAwaitingApproval
// derives the authoritative row outcome in its owned, generation-fenced UPDATE.
func (s *Service) validatePlanCrossCheckGateReason(ctx context.Context, q Store, worker store.Worker, lead store.Run, req StateRequest) error {
	reason := ""
	if req.PlanCrossCheckGateReason != nil {
		reason = *req.PlanCrossCheckGateReason
	}
	switch reason {
	case "", "revise", "block", "malformed", "model_error", "model_timeout",
		"checker_unavailable", "confinement_failed", "timed_out", "superseded",
		"codex_lead_unsupported", "planning_diff_refused", "interrupted":
	default:
		return ErrInvalidState
	}
	if req.PlanCrossCheckDiffRefusal != "" && reason != "planning_diff_refused" {
		return ErrInvalidState
	}
	if reason == "planning_diff_refused" {
		switch req.PlanCrossCheckDiffRefusal {
		case "base_unavailable", "diff_failed", "diff_too_large", "too_many_untracked", "secret_detected", "scan_failed":
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
	} // Stored outcome takes precedence over a declaration.
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
		harness := HarnessCodex
		_, err := s.resolveRunHarnessQ(ctx, lead.UserID, &harness, resolver)
		if errors.Is(err, ErrNoCredentialForHarness) || errors.Is(err, ErrHarnessCredentialDisabled) {
			return nil
		}
		if err != nil {
			return err
		}
	case "planning_diff_refused":
		// This bounded declaration attests capture failure; it never authorizes a pass.
		if lead.Harness != string(HarnessClaude) || worker.IsolatedLane {
			return ErrInvalidState
		}
		switch req.PlanCrossCheckDiffRefusal {
		case "base_unavailable", "diff_failed", "diff_too_large", "too_many_untracked", "secret_detected", "scan_failed":
			return nil
		}
	}
	return ErrInvalidState
}
