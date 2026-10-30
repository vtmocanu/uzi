package handler

import (
	"context"
	"log/slog"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// overlayLeadConfig sets RunDTO.LeadConfig for GetRun only (PRD #3244 section 5; a list read never
// runs it), when the run has a lead agent (leadcfg.LeadLane). Best-effort: a lookup error is
// logged and the field stays absent rather than failing the read of an otherwise-fine run.
func (h *Handler) overlayLeadConfig(ctx context.Context, run store.Run, dto *apitypes.RunDTO) {
	if !leadcfg.LeadLane(run.Kind, run.ReviewTargetRunID.Valid, run.EgressProfileID.Valid) {
		return
	}
	if h.q == nil { // handlers wired without a store (unit tests) have nothing to read
		return
	}
	rows, err := h.q.ListLatestRunLeadConfig(ctx, run.ID)
	if err != nil {
		slog.Error("run lead config", "run_id", run.ID, "error", err)
		return
	}
	dto.LeadConfig = leadConfigDTO(run, rows)
}

// leadConfigDTO builds the overlay from the latest-per-phase rows of the highest generation that
// has rows (plan first, as the query orders them). With no rows the claim generation is 0.
func leadConfigDTO(run store.Run, rows []store.RunLeadConfig) *apitypes.RunLeadConfigDTO {
	out := &apitypes.RunLeadConfigDTO{Applicable: true, Phases: make([]apitypes.LeadPhaseConfigDTO, 0, len(rows))}
	for _, r := range rows {
		out.ClaimGeneration = r.ClaimGeneration
		out.Phases = append(out.Phases, apitypes.LeadPhaseConfigDTO{
			Phase:                  r.Phase,
			PhaseEntry:             int(r.PhaseEntry),
			Seq:                    int(r.Seq),
			Harness:                r.Harness,
			RequestedModel:         textPtrValue(r.RequestedModel.Valid, r.RequestedModel.String),
			RequestedModelSource:   r.RequestedModelSource,
			RequestedModelFixedAt:  leadcfg.FixedAt(r.RequestedModelSource),
			RequestedEffort:        textPtrValue(r.RequestedEffort.Valid, r.RequestedEffort.String),
			RequestedEffortMode:    r.RequestedEffortMode,
			RequestedEffortSource:  r.RequestedEffortSource,
			RequestedEffortFixedAt: leadcfg.FixedAt(r.RequestedEffortSource),
			ObservedModel:          textPtrValue(r.ObservedModel.Valid, r.ObservedModel.String),
			ObservedModelEvidence:  textPtrValue(r.ObservedModelEvidence.Valid, r.ObservedModelEvidence.String),
			ObservedEffort:         textPtrValue(r.ObservedEffort.Valid, r.ObservedEffort.String),
			ObservedEffortEvidence: textPtrValue(r.ObservedEffortEvidence.Valid, r.ObservedEffortEvidence.String),
			Truncated:              r.Truncated,
			ReportedAt:             r.ReportedAt.Time,
		})
	}
	out.Current = out.ClaimGeneration == run.ClaimGeneration
	return out
}
