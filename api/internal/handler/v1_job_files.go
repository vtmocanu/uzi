package handler

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// v1_job_files.go serves the read side of job files (PRD #1909 M5): GET /api/v1/jobs/{id}/files and
// GET /api/v1/files/{id}. Authentication, the per-user limiter and the jobs:read scope are the
// route mount's (routes_v1.go).

const (
	// v1ReasonFileExpired is the reason of the 410 a download of an expired file answers.
	v1ReasonFileExpired = "file_expired"

	// A download's write deadline: the server's own WriteTimeout (15 s from the request start)
	// would cut off a large file, so the route extends it to a floor plus the time the file needs
	// at v1DownloadMinBytesPerSec, never over v1DownloadMaxDeadline.
	v1DownloadDeadlineFloor  = 2 * time.Minute
	v1DownloadMinBytesPerSec = 100 << 10
	v1DownloadMaxDeadline    = 10 * time.Minute
)

// V1JobFiles serves GET /api/v1/jobs/{id}/files: the job's inputs and outputs with metadata, and
// the outputs that were refused. A job the caller cannot see is 404, exactly as for the result.
func (h *Handler) V1JobFiles(w http.ResponseWriter, r *http.Request) {
	caller, ok := v1JobCaller(w, r)
	if !ok {
		return
	}
	id, ok := v1JobID(w, r)
	if !ok {
		return
	}
	view, err := h.wsvc.ListJobFilesForCaller(r.Context(), caller, id)
	if err != nil {
		writeV1JobError(w, "files", err)
		return
	}
	files, refused := v1JobFileDTOs(view)
	httpx.JSON(w, http.StatusOK, apitypes.V1JobFilesDTO{Files: files, RefusedFiles: refused})
}

// V1FileDownload serves GET /api/v1/files/{id}: the file's bytes, never rendered (D6). Every
// response carries Content-Disposition: attachment with a safe filename, X-Content-Type-Options:
// nosniff and Content-Type: application/octet-stream, whatever the file's detected type.
//
//	404 not_found         no such file, another user's, another product's, or a malformed id
//	410 file_expired      the file's retention ended; its bytes are gone
//	503 files_unavailable the deployment has no job-file store
//
// If the stored bytes fail their integrity check after the header is out, the response is torn
// (http.ErrAbortHandler) rather than ended cleanly, as the worker download does.
func (h *Handler) V1FileDownload(w http.ResponseWriter, r *http.Request) {
	caller, ok := v1JobCaller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorReason(w, http.StatusNotFound, "file not found", v1ReasonNotFound)
		return
	}
	jf := h.wsvc.JobFiles()
	if jf == nil {
		httpx.ErrorReason(w, http.StatusServiceUnavailable, "file downloads are not available on this server", v1ReasonFilesDisabled)
		return
	}
	f, body, err := jf.OpenForCaller(r.Context(), id, caller)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrJobFileNotFound):
			httpx.ErrorReason(w, http.StatusNotFound, "file not found", v1ReasonNotFound)
		case errors.Is(err, workersvc.ErrJobFileExpired):
			httpx.ErrorReason(w, http.StatusGone, "the file has expired", v1ReasonFileExpired)
		case errors.Is(err, workersvc.ErrJobFilesUnavailable):
			httpx.ErrorReason(w, http.StatusServiceUnavailable, "file downloads are not available on this server", v1ReasonFilesDisabled)
		case errors.Is(err, workersvc.ErrJobFileIntegrity):
			slog.Error("v1 file download integrity", "file", id.String())
			httpx.Error(w, http.StatusInternalServerError, "file failed its integrity check")
		default:
			slog.Error("v1 file download", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d := v1DownloadDeadlineFloor + time.Duration(f.ByteSize/v1DownloadMinBytesPerSec)*time.Second
	if d > v1DownloadMaxDeadline {
		d = v1DownloadMaxDeadline
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))

	hd := w.Header()
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("X-Content-Type-Options", "nosniff")
	// The filename is the content-derived storage name (<sha256>.<ext>: [0-9a-f.a-z] only), never
	// the uploader's display name, so the header needs no quoting or escaping and cannot carry
	// control or bidi characters. Fall back to the file id (a UUID, equally header-safe) when a row has no storage name.
	name := f.StorageName.String
	if name == "" {
		name = f.ID.String()
	}
	hd.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	hd.Set("Content-Length", strconv.FormatInt(f.ByteSize, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		if errors.Is(err, workersvc.ErrJobFileIntegrity) {
			slog.Error("v1 file download integrity failure mid-stream", "file", id.String())
		}
		panic(http.ErrAbortHandler)
	}
}

func v1JobFileDTOs(v workersvc.JobFilesView) ([]apitypes.V1JobFileDTO, []apitypes.V1JobRefusedFileDTO) {
	files := make([]apitypes.V1JobFileDTO, 0, len(v.Files))
	for _, f := range v.Files {
		files = append(files, apitypes.V1JobFileDTO{
			ID: f.ID.String(), DisplayName: f.DisplayName, ContentType: f.ContentType, ByteSize: f.ByteSize,
			Sha256: f.Sha256, Direction: f.Direction, State: f.State, ExpiresAt: f.ExpiresAt, SourceURL: f.SourceURL,
		})
	}
	refused := make([]apitypes.V1JobRefusedFileDTO, 0, len(v.Refused))
	for _, r := range v.Refused {
		refused = append(refused, apitypes.V1JobRefusedFileDTO{DisplayName: r.DisplayName, ByteSize: r.ByteSize, Reason: r.Reason})
	}
	return files, refused
}

func v1JobSourceDTOs(in []workersvc.JobSource) []apitypes.V1JobSourceDTO {
	out := make([]apitypes.V1JobSourceDTO, 0, len(in))
	for _, s := range in {
		out = append(out, apitypes.V1JobSourceDTO{
			URL: s.URL, FinalURL: s.FinalURL, Verdict: s.Verdict, Reason: s.Reason, HTTPStatus: s.HTTPStatus,
			ContentType: s.ContentType, ByteSize: s.ByteSize, Sha256: s.Sha256, FetchedAt: s.FetchedAt,
		})
	}
	return out
}
