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
	body, err := codeSnapshotCleanupBody(row)
	if err != nil {
		slog.Error("worker code snapshot cleanup", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, body)
}

// codeSnapshotCleanupBody maps the LEFT JOIN row to the wire shape. A lead with no code
// row, or a row with no child, is a valid answer carried by explicit flags. A row whose
// child mapping cannot be resolved is an error, never "no child": the worker must keep
// the ref when it cannot prove the checker is gone.
func codeSnapshotCleanupBody(row store.GetCodeSnapshotCleanupRow) (apitypes.CodeSnapshotCleanup, error) {
	body := apitypes.CodeSnapshotCleanup{
		Protocol: "code_snapshot_cleanup_v1", LeadRunID: row.LeadRunID.String(),
		LeadStatus: row.LeadStatus, OwnedByWorker: row.OwnedByWorker,
		RowPresent: row.RowPresent, LeadClaimGeneration: row.LeadClaimGeneration,
		ChildPresent: row.CheckerRunID.Valid, CheckerClaimGeneration: row.CheckerClaimGeneration,
	}
	if !row.RowPresent && (row.ChildMapped || row.CheckerRunID.Valid || row.Outcome.Valid) {
		return body, errors.New("code snapshot cleanup: child or outcome without a code row")
	}
	if row.RowPresent && !row.Outcome.Valid {
		return body, errors.New("code snapshot cleanup: code row without an outcome")
	}
	if row.ChildMapped != row.CheckerRunID.Valid {
		return body, errors.New("code snapshot cleanup: checker mapping does not resolve to a child run")
	}
	if row.HeadCommit.Valid {
		body.HeadCommit = &row.HeadCommit.String
	}
	if row.Outcome.Valid {
		body.Outcome = &row.Outcome.String
	}
	if row.CheckerRunID.Valid {
		id := uuid.UUID(row.CheckerRunID.Bytes).String()
		body.CheckerRunID = &id
		if !row.CheckerStatus.Valid {
			return body, errors.New("code snapshot cleanup: child run without a status")
		}
		body.CheckerStatus = &row.CheckerStatus.String
	}
	return body, nil
}
