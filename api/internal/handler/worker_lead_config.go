package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// maxLeadConfigBodyBytes bounds one lead-config report body (PRD #3244 section 4).
const maxLeadConfigBodyBytes = 4096

// WorkerRecordLeadConfig records one lead-config row the claim holder reports (PRD #3244).
// Status mapping:
//
//	400 claim_generation_required  claim_generation missing or below 1
//	400 invalid_lead_config        malformed or oversized body, unknown field, trailing value, any
//	                               leadcfg.ValidateReport failure, or a 23514 from the insert
//	                               matched by constraint name; the message names the field, never
//	                               the value
//	401                            no worker on the request
//	404                            run not held by this worker (ErrRunNotOwned)
//	409 claim_not_current          the fence admitted nothing
//	409 lead_config_key_conflict   the key exists with a different body (first row kept)
//	500                            anything else, including the store invariant breach
//	204                            inserted, or an identical row already existed
//
// A refresh frame is published to the hub only when a new row was committed. The body is never
// logged or echoed.
func (h *Handler) WorkerRecordLeadConfig(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req apitypes.LeadConfigReportRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, maxLeadConfigBodyBytes); err != nil {
		httpx.ErrorReason(w, http.StatusBadRequest, "invalid request body", "invalid_lead_config")
		return
	}
	rep, err := leadcfg.ValidateReport(req)
	if err != nil {
		var fe *leadcfg.FieldError
		if errors.As(err, &fe) {
			if fe.Field == "claim_generation" {
				httpx.ErrorReason(w, http.StatusBadRequest, "claim_generation is required", "claim_generation_required")
				return
			}
			httpx.ErrorReason(w, http.StatusBadRequest, "invalid lead config: "+fe.Field+" "+fe.Reason, "invalid_lead_config")
			return
		}
		slog.Error("worker record lead config: validate", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	res, err := h.wsvc.RecordLeadConfig(r.Context(), wkr, runID, rep)
	if err != nil {
		var cfe *workersvc.LeadConfigFieldError
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, store.ErrLeadConfigClaimNotCurrent):
			httpx.ErrorReason(w, http.StatusConflict, "the run's claim is not current", "claim_not_current")
		case errors.Is(err, store.ErrLeadConfigKeyConflict):
			httpx.ErrorReason(w, http.StatusConflict, "a different lead config row exists for this key", "lead_config_key_conflict")
		case errors.As(err, &cfe):
			httpx.ErrorReason(w, http.StatusBadRequest, "invalid lead config: "+cfe.Field+" is not acceptable", "invalid_lead_config")
		default:
			// A Postgres error's detail can quote the failing row, which holds worker-attested
			// text, so only its code and constraint are logged.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) {
				slog.Error("worker record lead config", "run_id", runID, "sqlstate", pgErr.Code, "constraint", pgErr.ConstraintName)
			} else {
				slog.Error("worker record lead config", "run_id", runID, "error", err)
			}
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if res.Inserted {
		// After commit only, hub only (never the MultiBroadcaster, so Slack sees nothing). A nil
		// hub is a no-op.
		h.hub.PublishRefresh(runID)
	}
	w.WriteHeader(http.StatusNoContent)
}
