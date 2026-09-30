package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// v1_jobs.go serves the /api/v1/jobs endpoints (PRD #1908 D10). Authentication, the per-user
// limiter and the scope check are the route mounts' (routes_v1.go); the handlers only turn the
// V1Principal into a workersvc.JobCaller and the service's typed errors into a status plus a
// stable machine `reason`.
//
// Error contract (api/openapi/v1.yaml documents each):
//
//	401 (no reason)              RequireV1Caller, one body whatever the cause
//	403 insufficient_scope       RequireScope
//	403 job_type_not_allowed     a product token whose product does not allow the type
//	404 not_found                no such job, another user's job, another product's job, or a
//	                             malformed id; the four are indistinguishable by design
//	409 job_terminal             cancel of a finished job
//	413 payload_too_large        create body over v1JobCreateMaxBodyBytes
//	422 invalid_request          malformed or invalid body or query (the message names the field)
//	422 unknown_job_type         type is not a known job type
//	422 not_supported            egress_profile (no egress control exists yet)
//	422 no_model_credential      the user has no usable Anthropic credential
//	422 file_unavailable         an input_file_ids entry cannot be attached (unknown, another
//	                             owner's or product's, already attached, expired, or listed twice)
//	413 too_many_files           more input_file_ids than UZI_JOB_INPUTS_MAX_FILES
//	413 job_bytes_exceeded       the attached files total more than UZI_JOB_INPUTS_MAX_BYTES
//	503 files_unavailable        input_file_ids was sent but this deployment has no file store
//	429 over_cap                 the user's non-terminal job cap
//	429 (no reason, Retry-After) the rate limiters
const (
	v1ReasonNotFound         = "not_found"
	v1ReasonInvalidRequest   = "invalid_request"
	v1ReasonUnknownJobType   = "unknown_job_type"
	v1ReasonNotSupported     = "not_supported"
	v1ReasonNoModelAccount   = "no_model_credential"
	v1ReasonTypeNotAllowed   = "job_type_not_allowed"
	v1ReasonOverCap          = "over_cap"
	v1ReasonJobTerminal      = "job_terminal"
	v1ReasonPayloadTooLarge  = "payload_too_large"
	v1JobCreateMaxBodyBytes  = 4 << 20 // prompt (256 KiB) + inputs (1 MiB) with JSON escaping headroom
	v1JobTitleFromPromptRune = 80
	// v1JobTitleMaxBytes mirrors workersvc's maxJobTitleBytes (unexported): the service refuses a
	// longer title, so a derived one must fit. TestV1JobTitleMaxBytesMatchesService pins the two
	// through the service's own validation.
	v1JobTitleMaxBytes = 200
)

// v1JobCaller builds the service caller from the resolved principal. ProductID and
// ProductTokenID are set ONLY for a uzp_ caller: a uzc_ caller's TokenID is a cli_tokens id and
// must never be recorded as a product token. It reports false, having written a 500, when the
// route was mounted without RequireV1Caller or a product principal carries no product.
func v1JobCaller(w http.ResponseWriter, r *http.Request) (workersvc.JobCaller, bool) {
	p, ok := mw.V1PrincipalFromContext(r.Context())
	if !ok || (p.Kind == mw.V1CallerProductToken && !p.ProductID.Valid) {
		// A programming error, deliberately not RequireV1Caller's 401 (see V1Whoami).
		slog.Error("v1 jobs: no usable V1Principal in context; /api/v1 route mounted without RequireV1Caller")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return workersvc.JobCaller{}, false
	}
	c := workersvc.JobCaller{UserID: p.User.ID}
	if p.Kind == mw.V1CallerProductToken {
		pid, tid := p.ProductID.UUID, p.TokenID
		c.ProductID, c.ProductTokenID = &pid, &tid
	}
	return c, true
}

// v1JobID parses the {id} path parameter. A malformed id is a 404 not_found, like an unknown one:
// it cannot name any job, and a distinct status would only add an oracle.
func v1JobID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorReason(w, http.StatusNotFound, "job not found", v1ReasonNotFound)
		return uuid.Nil, false
	}
	return id, true
}

// writeV1JobError maps a service error to its status and reason. Anything unrecognised is a
// logged 500 with no detail.
func writeV1JobError(w http.ResponseWriter, op string, err error) {
	var invalid *workersvc.JobInvalidError
	var refused *workersvc.JobFileRefusedError
	switch {
	case errors.As(err, &invalid):
		httpx.ErrorReason(w, http.StatusUnprocessableEntity, invalid.Error(), v1ReasonInvalidRequest)
	case errors.Is(err, workersvc.ErrJobInvalid):
		httpx.ErrorReason(w, http.StatusUnprocessableEntity, "the request is invalid", v1ReasonInvalidRequest)
	case errors.Is(err, workersvc.ErrJobTypeUnknown):
		httpx.ErrorReason(w, http.StatusUnprocessableEntity, "unknown job type", v1ReasonUnknownJobType)
	case errors.Is(err, workersvc.ErrJobNotSupported):
		httpx.ErrorReason(w, http.StatusUnprocessableEntity, "egress_profile is not supported yet", v1ReasonNotSupported)
	case workersvc.IsNoModelCredential(err):
		httpx.ErrorReason(w, http.StatusUnprocessableEntity, "the account has no usable model credential; add an Anthropic credential in uzi", v1ReasonNoModelAccount)
	case errors.Is(err, workersvc.ErrJobTypeNotAllowed):
		httpx.ErrorReason(w, http.StatusForbidden, "this token may not create jobs of this type", v1ReasonTypeNotAllowed)
	case errors.Is(err, workersvc.ErrJobOverCap):
		httpx.ErrorReason(w, http.StatusTooManyRequests, "too many active jobs; wait for one to finish or cancel one", v1ReasonOverCap)
	case errors.Is(err, workersvc.ErrJobFileUnavailable):
		httpx.ErrorReason(w, http.StatusUnprocessableEntity, "an input file is unavailable: it does not exist, is not yours, was uploaded through a different token, is already attached, has expired, or is listed twice", v1ReasonFileGone)
	case errors.As(err, &refused):
		writeV1FileRefusal(w, refused)
	case errors.Is(err, workersvc.ErrJobFilesUnavailable):
		httpx.ErrorReason(w, http.StatusServiceUnavailable, "file inputs are not available on this server", v1ReasonFilesDisabled)
	case errors.Is(err, workersvc.ErrJobNotFound):
		httpx.ErrorReason(w, http.StatusNotFound, "job not found", v1ReasonNotFound)
	case errors.Is(err, workersvc.ErrJobTerminal):
		httpx.ErrorReason(w, http.StatusConflict, "the job has already finished", v1ReasonJobTerminal)
	default:
		slog.Error("v1 jobs: "+op, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

func v1JobDTO(v workersvc.JobView) apitypes.V1JobDTO {
	return apitypes.V1JobDTO{
		ID:               v.ID.String(),
		Type:             v.JobType,
		Status:           v.Status,
		Title:            v.Title,
		RequestedByLabel: v.RequestedByLabel,
		FailureReason:    v.FailureReason,
		WallSeconds:      v.BudgetWallSeconds,
		CreatedAt:        v.CreatedAt,
		StartedAt:        v.StartedAt,
		FinishedAt:       v.FinishedAt,
	}
}

// decodeV1JobCreate reads the create body strictly: one JSON object, unknown fields refused, no
// trailing value, under v1JobCreateMaxBodyBytes. An oversize body is reported through the
// returned bool (a 413), every other failure is a 422 the caller writes.
func decodeV1JobCreate(w http.ResponseWriter, r *http.Request) (req apitypes.V1JobCreateRequest, tooLarge bool, err error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, v1JobCreateMaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return req, true, err
		}
		return req, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, false, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return req, false, errors.New("the body must contain exactly one JSON object")
	}
	return req, false, nil
}

// describeV1DecodeError words a body decode failure for the caller without echoing Go type or
// struct names (json's own text for a type mismatch names the internal struct).
func describeV1DecodeError(err error) string {
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	switch {
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return fmt.Sprintf("invalid request body: field %q has the wrong type", typeErr.Field)
		}
		return "invalid request body: the body has the wrong type"
	case errors.As(err, &syntaxErr), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "invalid request body: it is not valid JSON"
	default:
		// An unknown field (json: unknown field "x") and the trailing-value refusal name only
		// the offending key or the rule.
		return "invalid request body: " + err.Error()
	}
}

// derivedJobTitle is the title of a create request that named none: the prompt's first
// non-empty line, whitespace collapsed and cut to v1JobTitleFromPromptRune runes and
// v1JobTitleMaxBytes bytes, with any whitespace a cut leaves at the end trimmed. A prompt whose
// first line is not a displayable title (control or invisible characters) falls back to a
// generic one, so an omitted title never makes an otherwise valid request fail.
func derivedJobTitle(jobType, prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		t := strings.Join(fields, " ")
		if utf8.RuneCountInString(t) > v1JobTitleFromPromptRune {
			t = string([]rune(t)[:v1JobTitleFromPromptRune])
		}
		// Multi-byte runes can keep an 80-rune title over the service's byte limit: cut on a
		// rune boundary.
		for len(t) > v1JobTitleMaxBytes {
			_, size := utf8.DecodeLastRuneInString(t)
			t = t[:len(t)-size]
		}
		// A cut can land right after a space, and the validator refuses trailing whitespace.
		t = strings.TrimRightFunc(t, unicode.IsSpace)
		if t != "" && termsafe.Validate("title", t) == nil {
			return t
		}
		break
	}
	return jobType + " job"
}

// V1JobCreate serves POST /api/v1/jobs: 201 with the queued job.
func (h *Handler) V1JobCreate(w http.ResponseWriter, r *http.Request) {
	caller, ok := v1JobCaller(w, r)
	if !ok {
		return
	}
	req, tooLarge, err := decodeV1JobCreate(w, r)
	if tooLarge {
		httpx.ErrorReason(w, http.StatusRequestEntityTooLarge, "the request body is too large", v1ReasonPayloadTooLarge)
		return
	}
	if err != nil {
		httpx.ErrorReason(w, http.StatusUnprocessableEntity, describeV1DecodeError(err), v1ReasonInvalidRequest)
		return
	}
	params := workersvc.CreateJobParams{
		Caller:           caller,
		JobType:          req.Type,
		Prompt:           req.Prompt,
		RequestedByLabel: req.RequestedByLabel,
		WallSeconds:      req.WallSeconds,
		EgressProfile:    req.EgressProfile != nil,
	}
	if req.Title != nil {
		params.Title = *req.Title
	} else {
		params.Title = derivedJobTitle(req.Type, req.Prompt)
	}
	for _, in := range req.Inputs {
		params.Inputs = append(params.Inputs, workersvc.JobInput{Name: in.Name, Content: in.Content})
	}
	for i, raw := range req.InputFileIDs {
		id, perr := uuid.Parse(raw)
		if perr != nil {
			httpx.ErrorReason(w, http.StatusUnprocessableEntity, fmt.Sprintf("invalid input_file_ids[%d]: not a file id", i), v1ReasonInvalidRequest)
			return
		}
		params.InputFileIDs = append(params.InputFileIDs, id)
	}
	view, err := h.wsvc.CreateJobRun(r.Context(), params)
	if err != nil {
		writeV1JobError(w, "create", err)
		return
	}
	httpx.JSON(w, http.StatusCreated, v1JobDTO(view))
}

// V1JobGet serves GET /api/v1/jobs/{id}.
func (h *Handler) V1JobGet(w http.ResponseWriter, r *http.Request) {
	caller, ok := v1JobCaller(w, r)
	if !ok {
		return
	}
	id, ok := v1JobID(w, r)
	if !ok {
		return
	}
	view, err := h.wsvc.GetJobForCaller(r.Context(), caller, id)
	if err != nil {
		writeV1JobError(w, "get", err)
		return
	}
	httpx.JSON(w, http.StatusOK, v1JobDTO(view))
}

// v1JobCursorSep separates the timestamp and id inside the opaque list cursor.
const v1JobCursorSep = "|"

func encodeV1JobCursor(c workersvc.JobCursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + v1JobCursorSep + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeV1JobCursor(s string) (*workersvc.JobCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("cursor is not valid")
	}
	ts, id, ok := strings.Cut(string(raw), v1JobCursorSep)
	if !ok {
		return nil, errors.New("cursor is not valid")
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return nil, errors.New("cursor is not valid")
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return nil, errors.New("cursor is not valid")
	}
	return &workersvc.JobCursor{CreatedAt: t, ID: u}, nil
}

// V1JobList serves GET /api/v1/jobs?limit=&cursor=: the caller's visible jobs, newest first.
func (h *Handler) V1JobList(w http.ResponseWriter, r *http.Request) {
	caller, ok := v1JobCaller(w, r)
	if !ok {
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			httpx.ErrorReason(w, http.StatusUnprocessableEntity, "limit must be a whole number from 1 to 100", v1ReasonInvalidRequest)
			return
		}
		limit = n
	}
	var cursor *workersvc.JobCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeV1JobCursor(raw)
		if err != nil {
			httpx.ErrorReason(w, http.StatusUnprocessableEntity, err.Error(), v1ReasonInvalidRequest)
			return
		}
		cursor = c
	}
	views, next, err := h.wsvc.ListJobsForCaller(r.Context(), caller, cursor, limit)
	if err != nil {
		writeV1JobError(w, "list", err)
		return
	}
	out := apitypes.V1JobListDTO{Jobs: make([]apitypes.V1JobDTO, 0, len(views))}
	for _, v := range views {
		out.Jobs = append(out.Jobs, v1JobDTO(v))
	}
	if next != nil {
		c := encodeV1JobCursor(*next)
		out.NextCursor = &c
	}
	httpx.JSON(w, http.StatusOK, out)
}

// V1JobResult serves GET /api/v1/jobs/{id}/result: 200 for a visible job, with result null
// until one exists.
func (h *Handler) V1JobResult(w http.ResponseWriter, r *http.Request) {
	caller, ok := v1JobCaller(w, r)
	if !ok {
		return
	}
	id, ok := v1JobID(w, r)
	if !ok {
		return
	}
	res, err := h.wsvc.GetJobResult(r.Context(), caller, id)
	if err != nil {
		writeV1JobError(w, "result", err)
		return
	}
	out := apitypes.V1JobResultDTO{JobStatus: res.JobStatus}
	if res.Ready {
		body := &apitypes.V1JobResultBodyDTO{
			Status:   res.Status,
			ReportMd: res.ReportMD,
			Findings: make([]apitypes.V1JobFindingDTO, 0, len(res.Findings)),
		}
		for _, f := range res.Findings {
			body.Findings = append(body.Findings, apitypes.V1JobFindingDTO{
				Severity: f.Severity, MessageMd: f.MessageMD, URL: f.URL, File: f.File, Line: f.Line,
			})
		}
		out.Result = body
	}
	httpx.JSON(w, http.StatusOK, out)
}

// V1JobMessages serves GET /api/v1/jobs/{id}/messages?after=<seq>.
func (h *Handler) V1JobMessages(w http.ResponseWriter, r *http.Request) {
	caller, ok := v1JobCaller(w, r)
	if !ok {
		return
	}
	id, ok := v1JobID(w, r)
	if !ok {
		return
	}
	var after int32
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n < 0 {
			httpx.ErrorReason(w, http.StatusUnprocessableEntity, "after must be a non-negative whole number", v1ReasonInvalidRequest)
			return
		}
		after = int32(n)
	}
	msgs, err := h.wsvc.ListJobMessages(r.Context(), caller, id, after, 0)
	if err != nil {
		writeV1JobError(w, "messages", err)
		return
	}
	out := apitypes.V1JobMessagesDTO{Messages: make([]apitypes.V1JobMessageDTO, 0, len(msgs))}
	for _, m := range msgs {
		out.Messages = append(out.Messages, apitypes.V1JobMessageDTO{Seq: m.Seq, CreatedAt: m.CreatedAt, Type: m.Kind, Text: m.Text})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// V1JobCancel serves POST /api/v1/jobs/{id}/cancel: 200 with the job (a running job may still
// read running: its worker ends it asynchronously), 409 job_terminal for a finished one.
func (h *Handler) V1JobCancel(w http.ResponseWriter, r *http.Request) {
	caller, ok := v1JobCaller(w, r)
	if !ok {
		return
	}
	id, ok := v1JobID(w, r)
	if !ok {
		return
	}
	view, err := h.wsvc.CancelJob(r.Context(), caller, id)
	if err != nil {
		writeV1JobError(w, "cancel", err)
		return
	}
	httpx.JSON(w, http.StatusOK, v1JobDTO(view))
}
