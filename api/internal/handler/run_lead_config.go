package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
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
		out.Phases = append(out.Phases, leadPhaseConfigDTO(r))
	}
	out.Current = out.ClaimGeneration == run.ClaimGeneration
	return out
}

// leadPhaseConfigDTO maps one stored row to its DTO, deriving the fixed_at fields.
func leadPhaseConfigDTO(r store.RunLeadConfig) apitypes.LeadPhaseConfigDTO {
	return apitypes.LeadPhaseConfigDTO{
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
	}
}

// leadConfigHistoryCap is the most rows GET /api/runs/{id}/lead-config returns; the query fetches one
// more (LIMIT 257) so the handler can tell "exactly 256" from "more than 256".
const leadConfigHistoryCap = 256

// GetRunLeadConfigHistory serves GET /api/runs/{id}/lead-config (PRD #3244 M3): every recorded lead
// config row of the run, newest claim generation first. Owner-or-admin via GetRunForViewer (404
// otherwise, as GetRun). A run without a lead agent has no rows, so it answers an empty list.
func (h *Handler) GetRunLeadConfigHistory(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	if _, err := h.wsvc.GetRunForViewer(r.Context(), user.ID, user.IsAdmin, id); err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("get run lead config history", "run_id", id, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, err := h.q.ListRunLeadConfigHistory(r.Context(), id)
	if err != nil {
		slog.Error("list run lead config history", "run_id", id, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := apitypes.LeadConfigHistoryDTO{Entries: make([]apitypes.LeadConfigHistoryEntryDTO, 0, len(rows))}
	if len(rows) > leadConfigHistoryCap {
		out.Truncated = true
		rows = rows[:leadConfigHistoryCap]
	}
	for _, row := range rows {
		out.Entries = append(out.Entries, apitypes.LeadConfigHistoryEntryDTO{
			ClaimGeneration:    row.ClaimGeneration,
			LeadPhaseConfigDTO: leadPhaseConfigDTO(row),
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}
