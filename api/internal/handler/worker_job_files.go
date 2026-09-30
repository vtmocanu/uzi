package handler

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// WorkerJobInputFile streams one job input file to the worker holding the run (PRD #1909 D8):
// GET /worker/runs/{id}/files/{fileID}?claim_generation=N. The service enforces the fence (held by
// this worker, kind job, current claim generation, not terminal) and that the file is an attached
// input of THIS run; every other file reads as 404.
//
//	400 malformed id or claim_generation
//	403 not_for_job
//	404 run not held by this worker, or no such input file on this run
//	409 stale claim, or the run already finished
//	410 the file's bytes have expired
//
// The body is the plaintext with Content-Length and X-Uzi-File-Sha256. If the stored bytes fail
// their integrity check after the header is out, the connection is aborted (never a short 200 that
// looks complete): the reader surfaces workersvc.ErrJobFileIntegrity in place of io.EOF and the
// handler panics with http.ErrAbortHandler, which net/http turns into a torn connection.
func (h *Handler) WorkerJobInputFile(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	fileID, ok := httpx.PathUUID(w, r, "fileID", "file")
	if !ok {
		return
	}
	gen, err := strconv.ParseInt(r.URL.Query().Get("claim_generation"), 10, 64)
	if err != nil || gen < 0 {
		httpx.Error(w, http.StatusBadRequest, "claim_generation is required")
		return
	}
	f, body, err := h.wsvc.OpenJobInputFile(r.Context(), wkr, runID, fileID, gen)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound), errors.Is(err, workersvc.ErrJobFileNotFound):
			httpx.Error(w, http.StatusNotFound, "file not found")
		case errors.Is(err, workersvc.ErrNotJobRun):
			httpx.ErrorReason(w, http.StatusForbidden, "this route is only available for job runs", notForJobReason)
		case errors.Is(err, workersvc.ErrStaleClaim):
			httpx.JSON(w, http.StatusConflict, map[string]any{"disposition": "stale_claim"})
		case errors.Is(err, workersvc.ErrRunTerminal):
			httpx.Error(w, http.StatusConflict, "run has already finished")
		case errors.Is(err, workersvc.ErrJobFileExpired):
			httpx.Error(w, http.StatusGone, "file has expired")
		case errors.Is(err, workersvc.ErrJobFileIntegrity):
			slog.Error("worker job input file integrity", "run", runID.String(), "file", fileID.String())
			httpx.Error(w, http.StatusInternalServerError, "file failed its integrity check")
		default:
			slog.Error("worker job input file", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("X-Uzi-File-Sha256", f.Sha256.String)
	hd.Set("Content-Length", strconv.FormatInt(f.ByteSize, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		// Headers are out; the only honest signal left is a torn connection. A client disconnect
		// lands here too, and aborting it is harmless.
		if errors.Is(err, workersvc.ErrJobFileIntegrity) {
			slog.Error("worker job input file integrity failure mid-stream", "run", runID.String(), "file", fileID.String())
		}
		panic(http.ErrAbortHandler)
	}
}
