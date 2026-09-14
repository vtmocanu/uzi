package handler

import (
	"encoding/json"
	"errors"
	"io"
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
	case errors.Is(err, recovery.ErrAmbiguous):
		// PRD #1349 M4: a v1/no-generation worker holds more than one open hold, so the
		// server refuses to guess which generation the capture/release covers. The worker
		// (or owner) must name the exact generation; the holds stay open (fail closed).
		httpx.Error(w, http.StatusConflict, "ambiguous open custody generation; name the generation")
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

// WorkerListRecoveryHolds returns the caller worker's own open custody holds on the run — the
// post-clone generation-exact inventory (PRD #1349 M1, D3).
func (h *Handler) WorkerListRecoveryHolds(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	res, err := h.recovery().ListHoldsForWorkerRun(r.Context(), wkr, runID)
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
	// PRD #1349 M4: a v2 worker names its exact claim generation in the body so only that
	// generation's hold settles; a v1 worker sends no body (empty → io.EOF → Generation nil
	// → the server resolves a single open hold or, on ambiguity, retains). An empty body is
	// the backward-compatible v1 path, so io.EOF is not an error here.
	var req apitypes.RecoveryReleaseRequest
	if err := httpx.DecodeJSONStrict(r, &req); err != nil && !errors.Is(err, io.EOF) {
		httpx.Error(w, http.StatusBadRequest, "invalid release request")
		return
	}
	res, err := h.recovery().Release(r.Context(), wkr, runID, req)
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

// ListRecoveryHolds returns the caller's owner-wide custody holds + aggregate (PRD #1349 M5,
// D7). It is owner-scoped in SQL by the caller's user id (no run scope, no GetRun gate) — the
// board alert and Workers surface read the whole list, and `uzi run recovery` narrows by run
// client-side. Mounted under RequireUser so a session cookie OR a uzc_/uza_ CLI Bearer reach it.
func (h *Handler) ListRecoveryHolds(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	holds, err := h.recovery().ListHoldsForOwner(r.Context(), user.ID)
	if err != nil {
		slog.Error("recovery holds: list", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, holds)
}

// DiscardRecoveryHold discards ONE owner-owned open custody hold (PRD #1349 M5, D7/D9). It is
// the ONE mutating disposition of a held source and the ONLY mutating form of this route:
//  1. ?confirm=discard is validated BEFORE any SQL — a missing/different value is a fail-fast
//     400 with NO mutation, so an accidental DELETE can never destroy a possible only copy.
//  2. run + hold ids are parsed from the path.
//  3. ownership is strict owner-or-404 via h.wsvc.GetRun (admin refused, GetRun not
//     GetRunForViewer), exactly like DiscardRecoveryArchive.
//  4. the service runs the locked discard transaction (SQL re-verifies owner + open state).
//
// An available archive is NEVER deleted here (that is the separate DiscardRecoveryArchive
// choice); a foreign/absent/non-open hold returns {discarded:false} as a 404.
func (h *Handler) DiscardRecoveryHold(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// D7/D9: the explicit confirmation gate, BEFORE any SQL — this is the only mutating form,
	// so a missing or different value fails fast with no mutation.
	if r.URL.Query().Get("confirm") != "discard" {
		httpx.Error(w, http.StatusBadRequest, "hold discard requires ?confirm=discard")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	holdID, ok := httpx.PathUUID(w, r, "holdID", "hold")
	if !ok {
		return
	}
	// Strict owner-or-404, ignoring IsAdmin (mirrors DiscardRecoveryArchive): an admin acting
	// on a foreign run is refused. GetRun (not GetRunForViewer) is the authorization seam.
	if _, err := h.wsvc.GetRun(r.Context(), user.ID, runID); err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("recovery hold discard: get run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	discarded, err := h.recovery().DiscardHold(r.Context(), user.ID, runID, holdID)
	if err != nil {
		slog.Error("recovery hold discard", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !discarded {
		httpx.Error(w, http.StatusNotFound, "custody hold not found")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"discarded": true})
}
