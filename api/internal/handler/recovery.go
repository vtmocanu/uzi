package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// recoveryManifestHeader carries the RecoveryUploadManifest as compact JSON on the
// streaming upload request, so the bundle bytes are the pure octet-stream body and the
// manifest never has to be interleaved with them.
const recoveryManifestHeader = "X-Uzi-Recovery-Manifest"

// PRD #1296 M2 handlers (D2/D4/D6/D7). Worker-facing archive ops mount under the worker
// Bearer group (reserve/upload/status/release), authorized by the caller's worker identity
// against the capture's/hold's immutable original_worker_id; owner-facing ops mount under
// the RequireUser /runs group and each resolves ownership through h.wsvc.GetRun (owner-only,
// admin refused) exactly like ListRunInputs, then the recovery service's own owner-scoped
// queries.

// mapRecoveryError writes the HTTP status for a recovery service sentinel that occurred
// BEFORE any response body was written.
func mapRecoveryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, recovery.ErrBadRequest):
		httpx.Error(w, http.StatusBadRequest, "invalid recovery request")
	case errors.Is(err, recovery.ErrNotAuthorized):
		httpx.Error(w, http.StatusForbidden, "not authorized for this capture")
	case errors.Is(err, recovery.ErrCaptureNotFound):
		httpx.Error(w, http.StatusNotFound, "capture not found")
	case errors.Is(err, recovery.ErrNotAvailable):
		httpx.Error(w, http.StatusConflict, "capture is not available")
	case errors.Is(err, recovery.ErrOversize):
		httpx.Error(w, http.StatusRequestEntityTooLarge, "bundle exceeds the maximum size")
	case errors.Is(err, recovery.ErrManifestConflict):
		httpx.Error(w, http.StatusConflict, "a different manifest is already bound for this capture")
	case errors.Is(err, recovery.ErrQuota):
		httpx.Error(w, http.StatusInsufficientStorage, "storage quota exceeded")
	case errors.Is(err, recovery.ErrIntegrity):
		httpx.Error(w, http.StatusUnprocessableEntity, "archive integrity check failed")
	case errors.Is(err, recovery.ErrBusy):
		httpx.Error(w, http.StatusServiceUnavailable, "too many concurrent transfers; retry shortly")
	default:
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

// WorkerRecoveryReserve reserves (or idempotently re-reserves) a capture under the run's
// open custody hold held by the authenticated worker.
func (h *Handler) WorkerRecoveryReserve(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req apitypes.RecoveryReserveRequest
	if err := httpx.DecodeJSONStrict(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid reserve request")
		return
	}
	// The path {id} is the sole run identity; a body run_id, if present, must match it.
	if req.RunID != "" && req.RunID != runID.String() {
		httpx.Error(w, http.StatusBadRequest, "run id mismatch")
		return
	}
	res, err := h.recovery().Reserve(r.Context(), wkr, runID, req)
	if err != nil {
		mapRecoveryError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

// WorkerRecoveryUpload streams one octet-stream bundle into encrypted chunks and marks the
// capture ready in one transaction. The body is wrapped in http.MaxBytesReader so an
// over-cap body ERRORS (413) rather than being silently truncated.
func (h *Handler) WorkerRecoveryUpload(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	captureID, ok := httpx.PathUUID(w, r, "captureID", "capture")
	if !ok {
		return
	}
	raw := r.Header.Get(recoveryManifestHeader)
	if raw == "" {
		httpx.Error(w, http.StatusBadRequest, "missing recovery manifest header")
		return
	}
	var manifest apitypes.RecoveryUploadManifest
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid recovery manifest header")
		return
	}
	// Cap the ACTUAL streamed bytes: MaxBytesReader errors past the ceiling (io.LimitReader
	// would truncate). The service also verifies actual length == manifest size + checksum.
	body := http.MaxBytesReader(w, r.Body, h.cfg.RecoveryMaxBundleBytes)
	res, err := h.recovery().Upload(r.Context(), wkr, runID, captureID, manifest, body)
	if err != nil {
		mapRecoveryError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

// WorkerRecoveryStatus returns the worker's by-id status poll of a capture.
func (h *Handler) WorkerRecoveryStatus(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	captureID, ok := httpx.PathUUID(w, r, "captureID", "capture")
	if !ok {
		return
	}
	res, err := h.recovery().Status(r.Context(), wkr, runID, captureID)
	if err != nil {
		mapRecoveryError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

// WorkerRecoveryRelease releases the caller worker's open custody holds on the run.
func (h *Handler) WorkerRecoveryRelease(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	res, err := h.recovery().Release(r.Context(), wkr, runID)
	if err != nil {
		mapRecoveryError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

// ListRecoveryArchives returns the owner-scoped recovery summary + captures for a run.
func (h *Handler) ListRecoveryArchives(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	// Strict owner-or-404, ignoring IsAdmin (mirrors ListRunInputs): an admin viewing a
	// foreign run is refused. GetRun (not GetRunForViewer) is the authorization seam.
	if _, err := h.wsvc.GetRun(r.Context(), user.ID, runID); err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("recovery archives: get run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	summary, err := h.recovery().Summary(r.Context(), user.ID, runID)
	if err != nil {
		slog.Error("recovery archives: summary", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, summary)
}

// DownloadRecoveryArchive streams the decrypted bundle bytes of one owner-owned capture.
func (h *Handler) DownloadRecoveryArchive(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	captureID, ok := httpx.PathUUID(w, r, "captureID", "capture")
	if !ok {
		return
	}
	if _, err := h.wsvc.GetRun(r.Context(), user.ID, runID); err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("recovery download: get run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	err := h.recovery().Download(r.Context(), w, user.ID, runID, captureID)
	if err == nil {
		return
	}
	if errors.Is(err, recovery.ErrStreamAborted) {
		// The 200 response already began; the body is incomplete and the client detects
		// the short read. We cannot rewrite the status — only record the abort.
		slog.Error("recovery download aborted mid-stream",
			"run_id", runID.String(), "capture_id", captureID.String(), "user_id", user.ID.String())
		return
	}
	mapRecoveryError(w, err)
}

// DiscardRecoveryArchive marks one owner-owned capture discarded and deletes its bytes.
func (h *Handler) DiscardRecoveryArchive(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	captureID, ok := httpx.PathUUID(w, r, "captureID", "capture")
	if !ok {
		return
	}
	if _, err := h.wsvc.GetRun(r.Context(), user.ID, runID); err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("recovery discard: get run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	discarded, err := h.recovery().Discard(r.Context(), user.ID, runID, captureID)
	if err != nil {
		slog.Error("recovery discard", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !discarded {
		httpx.Error(w, http.StatusNotFound, "capture not found")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"discarded": true})
}
