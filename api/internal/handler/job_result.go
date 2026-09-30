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
	// RefusedOutputs are the outputs the worker dropped itself (empty, over its own ceilings,
	// unreadable, not uploaded); the api records each as a job_output_refusals row. Optional.
	RefusedOutputs []workerRefusedOutputBody `json:"refused_outputs"`
}

// workerRefusedOutputBody is one worker-side dropped output: the display name (sanitised by the
// upload-name path here) and a reason from the fixed workersvc.WorkerRefusalReasons allowlist.
type workerRefusedOutputBody struct {
	DisplayName string `json:"display_name"`
	Reason      string `json:"reason"`
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
	// Cheap ownership and kind check BEFORE the body is read, so a worker token cannot make the
	// api buffer and decode a multi-MiB body for a run it does not hold or that is not a job. The
	// in-transaction fence in SubmitJobResult stays the authority.
	if err := h.wsvc.CheckJobResultTarget(r.Context(), wkr, runID); err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.Is(err, workersvc.ErrNotJobRun):
			httpx.ErrorReason(w, http.StatusForbidden, "this route is only available for job runs", notForJobReason)
		default:
			slog.Error("worker job result target check", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	req, err := decodeJobResultBody(w, r)
	if err != nil {
		if errors.Is(err, errJobResultTooManyFindings) {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
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

// errJobResultTooManyFindings is decodeJobResultBody's refusal of a findings array longer than
// JobResultMaxFindings, raised while streaming, before the excess elements are materialised.
var errJobResultTooManyFindings = fmt.Errorf("at most %d findings", workersvc.JobResultMaxFindings)

// decodeJobResultBody reads one JSON object with unknown fields refused, under the job-result body
// cap (an over-cap body is an *http.MaxBytesError, answered 413 by RespondDecodeError).
func decodeJobResultBody(w http.ResponseWriter, r *http.Request) (workerJobResultRequest, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, jobResultMaxBodyBytes))
	if err != nil {
		return workerJobResultRequest{}, err
	}
	return decodeJobResultBytes(body)
}

// decodeJobResultBytes streams the findings array element by element and stops at the first
// element past JobResultMaxFindings, so a body of millions of `{}` elements never allocates them
// all (a plain struct decode would materialise every element before any count check). Every other
// field, and each finding, decodes strictly (unknown fields refused).
func decodeJobResultBytes(body []byte) (workerJobResultRequest, error) {
	var req workerJobResultRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return req, errors.New("request body must be a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return req, err
		}
		key, _ := keyTok.(string)
		// A repeated key is refused, never merged: findings in particular would otherwise be
		// appended across occurrences and get past the count check one array at a time.
		if seen[key] {
			return req, fmt.Errorf("duplicate field %q", key)
		}
		seen[key] = true
		switch key {
		case "claim_generation":
			err = dec.Decode(&req.ClaimGeneration)
		case "status":
			err = dec.Decode(&req.Status)
		case "report_md":
			err = dec.Decode(&req.ReportMd)
		case "findings":
			err = decodeJobFindingsStream(dec, &req.Findings)
		case "refused_outputs":
			err = decodeRefusedOutputsStream(dec, &req.RefusedOutputs)
		default:
			err = fmt.Errorf("unknown field %q", key)
		}
		if err != nil {
			return req, err
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return req, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return req, errors.New("request body must contain one JSON value")
	}
	return req, nil
}

func decodeJobFindingsStream(dec *json.Decoder, dst *[]workerJobFindingBody) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil { // findings: null
		return nil
	}
	if tok != json.Delim('[') {
		return errors.New("findings must be an array")
	}
	for dec.More() {
		if len(*dst) >= workersvc.JobResultMaxFindings {
			return errJobResultTooManyFindings
		}
		var f workerJobFindingBody
		if err := dec.Decode(&f); err != nil {
			return err
		}
		*dst = append(*dst, f)
	}
	_, err = dec.Token() // the closing bracket
	return err
}

// decodeRefusedOutputsStream reads the refused_outputs array element by element and stops at the
// first element past workersvc.JobResultMaxRefusedOutputs (the decodeJobFindingsStream discipline).
func decodeRefusedOutputsStream(dec *json.Decoder, dst *[]workerRefusedOutputBody) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil { // refused_outputs: null
		return nil
	}
	if tok != json.Delim('[') {
		return errors.New("refused_outputs must be an array")
	}
	for dec.More() {
		if len(*dst) >= workersvc.JobResultMaxRefusedOutputs {
			return fmt.Errorf("at most %d refused_outputs", workersvc.JobResultMaxRefusedOutputs)
		}
		var r workerRefusedOutputBody
		if err := dec.Decode(&r); err != nil {
			return err
		}
		*dst = append(*dst, r)
	}
	_, err = dec.Token() // the closing bracket
	return err
}

// validateRefusedOutputs checks and sanitises the worker-reported dropped outputs: the reason must
// be on the workersvc.WorkerRefusalReasons allowlist (a worker cannot write free text into a
// refusal row), the name goes through the same sanitiser as an uploaded file's, and a repeated
// (name, reason) is kept once. The order of first appearance is kept. An entry whose sanitised name
// is one of the server-generated names (workersvc.IsReservedJobOutputName) is dropped: those two
// files are the server's own, a worker never offers them, and a refusal row under one would read as
// the server's own output being refused.
func validateRefusedOutputs(in []workerRefusedOutputBody) ([]workersvc.JobRefusedOutput, error) {
	if len(in) > workersvc.JobResultMaxRefusedOutputs {
		return nil, fmt.Errorf("at most %d refused_outputs", workersvc.JobResultMaxRefusedOutputs)
	}
	out := make([]workersvc.JobRefusedOutput, 0, len(in))
	seen := map[workersvc.JobRefusedOutput]bool{}
	for i, r := range in {
		if !workersvc.WorkerRefusalReasons[r.Reason] {
			return nil, fmt.Errorf("refused_outputs[%d]: unknown reason", i)
		}
		if len(r.DisplayName) > 4*v1FileDisplayNameMaxBytes {
			return nil, fmt.Errorf("refused_outputs[%d]: display_name is too long", i)
		}
		e := workersvc.JobRefusedOutput{DisplayName: sanitizeUploadName(r.DisplayName), Reason: r.Reason}
		if workersvc.IsReservedJobOutputName(e.DisplayName) {
			continue
		}
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out, nil
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
	refused, err := validateRefusedOutputs(req.RefusedOutputs)
	if err != nil {
		return workersvc.JobResultSubmission{}, err
	}
	sub.RefusedOutputs = refused
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
		// Cap after the scrub: the redactor can grow the string and the bounder can overshoot on a
		// multibyte rune, and the DB octet_length(url) <= 2048 CHECK would 500 the whole ingest.
		u := capUTF8(scrubThenBoundSelfReported(*f.URL, workersvc.JobFindingURLMaxBytes), workersvc.JobFindingURLMaxBytes)
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
