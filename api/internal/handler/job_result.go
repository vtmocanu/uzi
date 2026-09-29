package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// jobResultMaxBodyBytes bounds the job-result request body. The report alone may be 1 MiB, so the
// generic 1 MiB decoder cap would refuse a legal maximal report; findings add at most a few MiB
// more, and the per-field caps below are the real bounds.
const jobResultMaxBodyBytes = 4 << 20

// jobResultStatusRE is the shape of the result status token: short, lowercase, snake or kebab.
var jobResultStatusRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// workerJobResultRequest is the job runner's POST (PRD #1908). Every free-text field is
// UNTRUSTED: the handler enum-validates, caps, control-strips and secret-scrubs before it reaches
// the DB.
type workerJobResultRequest struct {
	// ClaimGeneration is the job flight's claim generation; REQUIRED, the fence against a stale
	// flight of the same run.
	ClaimGeneration *int64                 `json:"claim_generation"`
	Status          string                 `json:"status"`
	ReportMd        string                 `json:"report_md"`
	Findings        []workerJobFindingBody `json:"findings"`
}

type workerJobFindingBody struct {
	Severity  string  `json:"severity"`
	MessageMd string  `json:"message_md"`
	URL       *string `json:"url"`
	File      *string `json:"file"`
	Line      *int64  `json:"line"`
}

// WorkerJobResult ingests a job run's structured result: POST /worker/runs/{id}/job-result, {id}
// being the job run the calling worker holds. Ownership, kind, the claim generation and the atomic
// replace-upsert live in the service; here the strict body is validated and scrubbed.
//
//	400 invalid body (unknown field, bad enum, oversize field, bad url/location, missing generation)
//	403 not_for_job (the run is not a job run)
//	404 run not held by this worker
//	409 stale claim, or the run already finished
//	413 body too large
func (h *Handler) WorkerJobResult(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req workerJobResultRequest
	if err := decodeJobResultBody(w, r, &req); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	if req.ClaimGeneration == nil {
		httpx.Error(w, http.StatusBadRequest, "claim_generation is required")
		return
	}
	sub, err := validateAndScrubJobResult(req)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.wsvc.SubmitJobResult(r.Context(), wkr, runID, *req.ClaimGeneration, sub); err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.Is(err, workersvc.ErrNotJobRun):
			httpx.ErrorReason(w, http.StatusForbidden, "this route is only available for job runs", notForJobReason)
		case errors.Is(err, workersvc.ErrStaleClaim):
			httpx.JSON(w, http.StatusConflict, map[string]any{"disposition": "stale_claim"})
		case errors.Is(err, workersvc.ErrRunTerminal):
			httpx.Error(w, http.StatusConflict, "run has already finished")
		default:
			slog.Error("worker job result", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// decodeJobResultBody reads one JSON value with unknown fields refused, under the job-result body
// cap (an over-cap body is an *http.MaxBytesError, answered 413 by RespondDecodeError).
func decodeJobResultBody(w http.ResponseWriter, r *http.Request, dst any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, jobResultMaxBodyBytes))
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

// validateAndScrubJobResult is the job-result ingest gate (PRD #1908, the task-review discipline):
// reject a bad status token, an over-cap count or field, an unknown severity, a finding that names
// both a url and a file or a line without a file, or a non-http(s) url; strip control characters
// (keeping markdown newlines in the multi-line fields), secret-scrub every free field, and only
// then bound it, before anything is persisted or rendered. Oversize input is REJECTED, never
// silently cut; the post-scrub bound only absorbs the redactor's own growth.
func validateAndScrubJobResult(req workerJobResultRequest) (workersvc.JobResultSubmission, error) {
	if !jobResultStatusRE.MatchString(req.Status) {
		return workersvc.JobResultSubmission{}, errors.New("status must be a short lowercase token")
	}
	if len(req.ReportMd) > workersvc.JobResultReportMaxBytes {
		return workersvc.JobResultSubmission{}, fmt.Errorf("report_md exceeds %d bytes", workersvc.JobResultReportMaxBytes)
	}
	if len(req.Findings) > workersvc.JobResultMaxFindings {
		return workersvc.JobResultSubmission{}, fmt.Errorf("at most %d findings", workersvc.JobResultMaxFindings)
	}
	sub := workersvc.JobResultSubmission{
		Status:   req.Status,
		ReportMD: capUTF8(scrubThenBoundMarkdown(req.ReportMd, workersvc.JobResultReportMaxBytes), workersvc.JobResultReportMaxBytes),
		Findings: make([]workersvc.JobFindingSubmission, 0, len(req.Findings)),
	}
	for i, f := range req.Findings {
		fs, err := validateAndScrubJobFinding(f)
		if err != nil {
			return workersvc.JobResultSubmission{}, fmt.Errorf("findings[%d]: %w", i, err)
		}
		sub.Findings = append(sub.Findings, fs)
	}
	return sub, nil
}

func validateAndScrubJobFinding(f workerJobFindingBody) (workersvc.JobFindingSubmission, error) {
	if !workersvc.JobFindingSeverities[f.Severity] {
		return workersvc.JobFindingSubmission{}, fmt.Errorf("severity must be one of info|warning|error, got %q", f.Severity)
	}
	if len(f.MessageMd) > workersvc.JobFindingMessageMaxBytes {
		return workersvc.JobFindingSubmission{}, fmt.Errorf("message_md exceeds %d bytes", workersvc.JobFindingMessageMaxBytes)
	}
	msg := capUTF8(scrubThenBoundMarkdown(f.MessageMd, workersvc.JobFindingMessageMaxBytes), workersvc.JobFindingMessageMaxBytes)
	if msg == "" {
		return workersvc.JobFindingSubmission{}, errors.New("message_md must not be empty")
	}
	out := workersvc.JobFindingSubmission{Severity: f.Severity, MessageMD: msg}
	if f.URL != nil && f.File != nil {
		return workersvc.JobFindingSubmission{}, errors.New("a finding names a url or a file, not both")
	}
	if f.Line != nil && f.File == nil {
		return workersvc.JobFindingSubmission{}, errors.New("line requires file")
	}
	if f.URL != nil {
		if len(*f.URL) > workersvc.JobFindingURLMaxBytes {
			return workersvc.JobFindingSubmission{}, fmt.Errorf("url exceeds %d bytes", workersvc.JobFindingURLMaxBytes)
		}
		u := scrubThenBoundSelfReported(*f.URL, workersvc.JobFindingURLMaxBytes)
		if err := validateJobFindingURL(u); err != nil {
			return workersvc.JobFindingSubmission{}, err
		}
		out.URL = &u
	}
	if f.File != nil {
		if len(*f.File) > workersvc.JobFindingFileMaxBytes {
			return workersvc.JobFindingSubmission{}, fmt.Errorf("file exceeds %d bytes", workersvc.JobFindingFileMaxBytes)
		}
		file := capUTF8(scrubThenBoundSelfReported(*f.File, workersvc.JobFindingFileMaxBytes), workersvc.JobFindingFileMaxBytes)
		if file == "" {
			return workersvc.JobFindingSubmission{}, errors.New("file must not be empty")
		}
		out.File = &file
	}
	if f.Line != nil {
		if *f.Line < 0 || *f.Line > workersvc.JobFindingMaxLine {
			return workersvc.JobFindingSubmission{}, fmt.Errorf("line must be between 0 and %d", workersvc.JobFindingMaxLine)
		}
		line := int32(*f.Line) // #nosec G115 -- bounded by JobFindingMaxLine above
		out.Line = &line
	}
	return out, nil
}

// validateJobFindingURL admits an absolute http or https URL with a host and no userinfo, parsed
// with net/url. Any other scheme (javascript:, data:, file:, ...) is refused.
func validateJobFindingURL(raw string) error {
	if raw == "" {
		return errors.New("url must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("url must be an http or https URL")
	}
	if u.Host == "" || u.Hostname() == "" {
		return errors.New("url must include a host")
	}
	if u.User != nil {
		return errors.New("url must not carry credentials")
	}
	return nil
}

// capUTF8 cuts s to at most max bytes on a rune boundary. termsafe.SanitizeBounded stops after the
// rune that reaches max, so it can overshoot by up to three bytes; the DB octet CHECKs do not.
func capUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return strings.TrimSpace(s)
}
