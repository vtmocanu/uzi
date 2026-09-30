package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// jobFileMetaHeader carries the JSON metadata of one output upload, so the request body is the
// file's raw bytes (the X-Uzi-Recovery-Manifest pattern, handler/recovery.go).
const jobFileMetaHeader = "X-Uzi-Job-File"

// workerJobOutputFallbackDeadline is the read deadline when no job-file store is wired (the request
// is then refused after the fence and never streams).
const workerJobOutputFallbackDeadline = 120 * time.Second

// jobFileMetaMaxBytes bounds the metadata header: the JSON is a generation, a name of at most a few
// hundred bytes after escaping, a size and a 64-character digest.
const jobFileMetaMaxBytes = 2048

// workerJobFileMeta is the X-Uzi-Job-File JSON: claim_generation (the fence, REQUIRED), the
// display name (sanitised again here), the exact size in bytes and the sha256 of the bytes.
type workerJobFileMeta struct {
	ClaimGeneration *int64 `json:"claim_generation"`
	DisplayName     string `json:"display_name"`
	Size            *int64 `json:"size"`
	SHA256          string `json:"sha256"`
}

// WorkerJobOutputFile stores one output file of a job run: POST /worker/runs/{id}/files, {id} being
// the job run the calling worker holds (PRD #1909 D4, D6). The body is the raw file; the metadata
// is the X-Uzi-Job-File header. The fence (held by this worker, kind job, current claim generation
// and not released, not terminal) runs BEFORE any of the body is read, and the file is reserved at
// exactly the declared size, then streamed under http.MaxBytesReader(declared size) and a read
// deadline for this route.
//
//	201 the stored file (V1FileDTO: id, storage_name, content_type, byte_size, sha256, ...)
//	200 the same, when a retry finds the file its first attempt stored (nothing is stored twice)
//	400 missing or malformed header, or an unreadable body
//	403 not_for_job
//	404 run not held by this worker
//	408 the body was not received within the request deadline
//	409 stale claim, or the run already finished
//	413 a limit: file_too_large, too_many_files or job_bytes_exceeded (recorded as a refusal)
//	415 unsupported_file_type (recorded as a refusal)
//	422 empty_file, size_mismatch, sha256_mismatch (recorded as a refusal, reservation released)
//	503 files_unavailable, or uploads_busy with Retry-After
//	507 storage_quota_exceeded: the owner's, the instance's or the shared budget is full (recorded)
//
// A refusal never fails the job: the worker logs it and goes on to the next file.
func (h *Handler) WorkerJobOutputFile(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	meta, ok := parseJobFileMeta(w, r)
	if !ok {
		return
	}
	jf := h.wsvc.JobFiles()
	// The limits are only needed for the deadline; a missing store is answered by the service
	// AFTER its fence, so a worker that does not hold the run learns nothing from it.
	deadlineD := workerJobOutputFallbackDeadline
	if jf != nil {
		deadlineD = jf.Limits().RequestDeadline
	}
	deadline := setJobFileDeadlines(w, deadlineD)

	name := sanitizeUploadName(meta.DisplayName)
	body := http.MaxBytesReader(w, r.Body, max(*meta.Size, 1))
	res, err := h.wsvc.StoreJobOutput(r.Context(), wkr, runID, workersvc.JobOutputParams{
		ClaimGeneration: *meta.ClaimGeneration,
		DisplayName:     name,
		Size:            *meta.Size,
		SHA256:          meta.SHA256,
	}, body, workersvc.WriteOptions{
		Inspector: &v1UploadInspector{declared: v1OutputDeclaredTypes(name), allowHTML: true},
	})
	if err != nil {
		var ref *workersvc.JobFileRefusedError
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.Is(err, workersvc.ErrNotJobRun):
			httpx.ErrorReason(w, http.StatusForbidden, "this route is only available for job runs", notForJobReason)
		case errors.Is(err, workersvc.ErrStaleClaim):
			httpx.JSON(w, http.StatusConflict, map[string]any{"disposition": "stale_claim"})
		case errors.Is(err, workersvc.ErrRunTerminal):
			httpx.Error(w, http.StatusConflict, "run has already finished")
		case errors.Is(err, workersvc.ErrJobFileUploadsBusy):
			w.Header().Set("Retry-After", strconv.Itoa(v1FileBusyRetryAfter))
			httpx.ErrorReason(w, http.StatusServiceUnavailable, "too many uploads are in progress; retry shortly", v1ReasonUploadsBusy)
		case errors.As(err, &ref):
			writeWorkerOutputRefusal(w, ref)
		default:
			writeV1FileError(w, "worker output", err, deadline)
		}
		return
	}
	status := http.StatusCreated
	if res.Existing {
		status = http.StatusOK
	}
	httpx.JSON(w, status, v1FileDTO(res.File))
}

// parseJobFileMeta reads and validates the X-Uzi-Job-File header, answering 400 itself.
func parseJobFileMeta(w http.ResponseWriter, r *http.Request) (workerJobFileMeta, bool) {
	raw := r.Header.Get(jobFileMetaHeader)
	if raw == "" || len(raw) > jobFileMetaMaxBytes {
		httpx.Error(w, http.StatusBadRequest, "missing or oversized "+jobFileMetaHeader+" header")
		return workerJobFileMeta{}, false
	}
	var meta workerJobFileMeta
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&meta); err != nil || dec.More() {
		httpx.Error(w, http.StatusBadRequest, "invalid "+jobFileMetaHeader+" header")
		return workerJobFileMeta{}, false
	}
	switch {
	case meta.ClaimGeneration == nil || *meta.ClaimGeneration < 0:
		httpx.Error(w, http.StatusBadRequest, "claim_generation is required")
	case meta.Size == nil || *meta.Size < 0:
		httpx.Error(w, http.StatusBadRequest, "size is required")
	case !v1SHA256Hex(meta.SHA256):
		httpx.Error(w, http.StatusBadRequest, "sha256 must be 64 lowercase hexadecimal characters")
	default:
		return meta, true
	}
	return workerJobFileMeta{}, false
}

// writeWorkerOutputRefusal maps a refused output to its status; the JSON body carries the stable
// reason (workersvc.Refusal*) the worker logs. The job is never failed by any of these.
func writeWorkerOutputRefusal(w http.ResponseWriter, ref *workersvc.JobFileRefusedError) {
	switch ref.Kind {
	case workersvc.RefusalLimit:
		httpx.ErrorReason(w, http.StatusRequestEntityTooLarge, "the output file is over a size or count limit", ref.Reason)
	case workersvc.RefusalQuota:
		httpx.ErrorReason(w, http.StatusInsufficientStorage, "stored-file storage is full", ref.Reason)
	default:
		switch ref.Reason {
		case v1ReasonUnsupported, workersvc.RefusalUnsupported, workersvc.RefusalContentInvalid:
			msg := "the output file is not a supported type, or its content does not match its type"
			if ref.Detail != "" {
				msg += ": " + ref.Detail
			}
			httpx.ErrorReason(w, http.StatusUnsupportedMediaType, msg, ref.Reason)
		default:
			slog.Debug("worker output refused", "reason", ref.Reason)
			httpx.ErrorReason(w, http.StatusUnprocessableEntity, "the output file was refused", ref.Reason)
		}
	}
}
