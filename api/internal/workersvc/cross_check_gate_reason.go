package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
		active := (lead.AutoApprove || lead.PlanCrossCheckGated) && (lead.Status == "claimed" || lead.Status == "running")
		current := fencedGate && lead.ClaimGeneration > 0 && req.PresentationID != nil &&
			lead.GatePresentationID.Valid && uuid.UUID(lead.GatePresentationID.Bytes) == *req.PresentationID
		retained := lead.Status == "awaiting_approval" && !lead.AutoApprove &&
			(lead.PlanCrossCheckGateReason.String == reason || current)
		if (!lead.PlanCrossCheckRequired && !lead.PlanCrossCheckGated) || (!active && !retained) || !isCrossCheckLeadHarness(lead.Harness) ||
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
		// PRD #4574 M2: the same checkerLaneDecision the creation used, over a fresh read of the lead's
		// frozen binding and the owner's sources. Refused is the server's own fact (the stage lands on
		// the human gate; a non-pass never auto-approves). Provider is attested only when its source
		// is unusable, since no child exists at attestation; a usable source has a checker lane, and
		// a source that vanished is no fact to attest. Only Vendor runs the vendor-resolution check.
		harness, known := oppositeHarness(lead.Harness)
		if !known {
			return ErrInvalidState
		}
		sources, serr := resolver.GetUserProviderLaneSources(ctx, lead.UserID)
		if serr != nil {
			return serr
		}
		switch checkerLaneDecision(providerBoundRun(lead), harness, sources) {
		case checkerLaneRefused:
			return nil
		case checkerLaneProvider:
			src, perr := ResolveProviderSource(ctx, resolver, lead.UserID, string(harness), s.providerAllow)
			if perr != nil {
				return perr
			}
			if src.State == ProviderSourceUnusable {
				return nil
			}
			return ErrInvalidState
		}
		_, err := s.resolveRunHarnessMode(ctx, lead.UserID, &harness, resolver, true)
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

// gatedPlanApprovalConstraint names the constraint the enforce_gated_plan_approval trigger
// (migration 00394) raises, with SQLSTATE 23514, when a gated run's awaiting_approval ->
// running edge has no current bound approval.
const gatedPlanApprovalConstraint = "runs_gated_plan_approval"

// isGatedPlanApprovalViolation reports whether err is that trigger's refusal.
func isGatedPlanApprovalViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == gatedPlanApprovalConstraint
}
