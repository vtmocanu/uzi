package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func (h *Handler) workerCodeSnapshotCleanup(w http.ResponseWriter, r *http.Request, worker store.Worker, runID uuid.UUID) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["purpose"]) != 1 || query["purpose"][0] != "code_snapshot" {
		httpx.Error(w, http.StatusBadRequest, "purpose must be exactly code_snapshot")
		return
	}
	if !slices.Contains(worker.ProtocolCapabilities, "cross_check_code_v1") {
		httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		return
	}
	row, err := h.q.GetCodeSnapshotCleanup(r.Context(), store.GetCodeSnapshotCleanupParams{
		LeadRunID: runID, UserID: worker.UserID, WorkerID: worker.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		slog.Error("worker code snapshot cleanup", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !row.Outcome.Valid {
		httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		return
	}
	var head *string
	if row.HeadCommit.Valid {
		head = &row.HeadCommit.String
	}
	httpx.JSON(w, http.StatusOK, apitypes.CodeSnapshotCleanup{
		Protocol: "code_snapshot_cleanup_v1", LeadRunID: row.LeadRunID.String(),
		HeadCommit: head, Outcome: row.Outcome.String, LeadStatus: row.LeadStatus,
		OwnedByWorker: row.OwnedByWorker, CheckerRunID: row.CheckerRunID.String(),
		CheckerClaimGeneration: row.CheckerClaimGeneration, CheckerStatus: row.CheckerStatus,
	})
}
