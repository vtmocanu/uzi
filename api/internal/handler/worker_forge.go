package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Forge read caps (PRD #158 M1). The server enforces these regardless of what the
// client asks: MaxForgeListItems bounds every list route (issues, label-events,
// jobs) and MaxForgeBodyBytes bounds a single issue's description. Both are hard
// server-side truncation of the RESPONSE, not advisory — a compromised worker
// cannot ask for more.
//
// Caveat (PRD #158, accepted residual): only list_issues also bounds the UPSTREAM
// fetch — it passes MaxForgeListItems+1 as the driver Limit so the whole-project
// walk stops early. label-events and jobs use driver methods with no Limit (shared
// with autopilot/ci_fix, which need complete sets), so they fetch the issue's /
// pipeline's full set into memory and truncate to MaxForgeListItems AFTER. That set
// is per-issue / per-pipeline scoped and own-project + write-gated + capped by the
// agent-side per-session call budget, so it is a bounded amplification, not a
// whole-project enumeration; a numeric upstream cap on those two is a follow-up.
const (
	// MaxForgeListItems caps the number of rows any forge list route returns. The
	// list_issues route asks the driver for one MORE than this so a full page can be
	// reported as truncated=true without a second round trip.
	MaxForgeListItems = 50
	// MaxForgeBodyBytes caps an issue description in the single-issue GET, byte-safe
	// on a UTF-8 boundary.
	MaxForgeBodyBytes = 32768
)

// Fixed, coordinate-free error strings (PRD #158 Success Criterion 3). The forge SDK
// errors embed the request URL (host + projects/<id>) and forge/redact.go scrubs only
// the PAT, so the driver's err.Error() is NEVER put in the response body — the real
// (PAT-redacted) error is logged server-side and the client gets one of these.
const (
	forgeErrAuth        = "worker authentication required"
	forgeErrRunNotFound = "run not found"
	forgeErrNoRepo      = "run has no repository"
	forgeErrInvalid     = "invalid request"
	forgeErrUpstream    = "could not read from the forge"
)

// resolveForgeRun authorizes the worker against the run named by the {id} path param
// and builds a forge driver for it. It writes the fixed error response and returns
// ok=false on any failure (401 no worker, 400 bad run id, 404 not owned, 409 repoless,
// 502 driver-build failure), so the caller returns immediately on !ok.
func (h *Handler) resolveForgeRun(w http.ResponseWriter, r *http.Request) (workersvc.ForgeConn, forge.Forge, bool) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, forgeErrAuth)
		return workersvc.ForgeConn{}, nil, false
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, forgeErrInvalid)
		return workersvc.ForgeConn{}, nil, false
	}
	conn, err := h.wsvc.ForgeConnForRun(r.Context(), wkr, runID)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, forgeErrRunNotFound)
		case errors.Is(err, workersvc.ErrForgeNoRepo):
			httpx.Error(w, http.StatusConflict, forgeErrNoRepo)
		default:
			slog.Error("worker forge conn", "error", err)
			httpx.Error(w, http.StatusBadGateway, forgeErrUpstream)
		}
		return workersvc.ForgeConn{}, nil, false
	}
	f, err := h.svc.ForgeForConnection(conn.ForgeType, conn.BaseUrl, conn.TokenCiphertext)
	if err != nil {
		// A build/decrypt failure is coordinate-free already, but map it to the same
		// generic upstream error so the worker never sees an internal detail either.
		slog.Error("worker forge build driver", "error", err)
		httpx.Error(w, http.StatusBadGateway, forgeErrUpstream)
		return workersvc.ForgeConn{}, nil, false
	}
	return conn, f, true
}

// forgeDriverError logs the (already PAT-redacted) driver error server-side and
// answers with the fixed generic 502 — the URL the SDK embeds (host + project id)
// must never reach the agent.
func (h *Handler) forgeDriverError(w http.ResponseWriter, err error) {
	slog.Error("worker forge read", "error", err)
	httpx.Error(w, http.StatusBadGateway, forgeErrUpstream)
}

// rfc3339 formats a forge timestamp as an RFC3339 string in UTC. A zero time (the
// forge omitted the field) formats to the RFC3339 zero value, which the TS client
// treats as "unknown".
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// nonNilStrings returns s, or an empty non-nil slice when s is nil, so the labels
// field marshals as [] rather than null.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// WorkerForgeGetIssue reads one issue (with its possibly-truncated description) from
// the run's forge. GET /worker/runs/{id}/forge/issues/{iid} (PRD #158 M1).
func (h *Handler) WorkerForgeGetIssue(w http.ResponseWriter, r *http.Request) {
	conn, f, ok := h.resolveForgeRun(w, r)
	if !ok {
		return
	}
	iid, err := parseInt64(chi.URLParam(r, "iid"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, forgeErrInvalid)
		return
	}
	issue, err := f.GetIssue(r.Context(), conn.ForgeProjectID, iid)
	if err != nil {
		h.forgeDriverError(w, err)
		return
	}
	desc, truncated := truncateForgeBody(issue.Description)
	httpx.JSON(w, http.StatusOK, apitypes.ForgeIssueDTO{
		IID:                  issue.IID,
		Title:                issue.Title,
		State:                issue.State,
		Labels:               nonNilStrings(issue.Labels),
		Author:               issue.Author,
		UpdatedAt:            rfc3339(issue.UpdatedAt),
		Description:          desc,
		DescriptionTruncated: truncated,
	})
}

// WorkerForgeListIssues reads a capped, filtered issue list from the run's forge.
// GET /worker/runs/{id}/forge/issues?state=&labels=&updated_after=&limit= (PRD #158 M1).
func (h *Handler) WorkerForgeListIssues(w http.ResponseWriter, r *http.Request) {
	conn, f, ok := h.resolveForgeRun(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var opts forge.ListIssuesOptions
	switch q.Get("state") {
	case "":
		opts.State = forge.StateAll
	case "opened":
		opts.State = forge.StateOpened
	case "closed":
		opts.State = forge.StateClosed
	default:
		httpx.Error(w, http.StatusBadRequest, forgeErrInvalid)
		return
	}
	if raw := q.Get("labels"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				opts.Labels = append(opts.Labels, p)
			}
		}
	}
	if raw := q.Get("updated_after"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, forgeErrInvalid)
			return
		}
		opts.UpdatedAfter = &t
	}
	// The client's `limit` is advisory and deliberately ignored: the server always
	// asks the driver for MaxForgeListItems+1 so it can both cap the response and
	// report truncated=true, regardless of what the worker requested.
	opts.Limit = MaxForgeListItems + 1

	issues, err := f.ListIssues(r.Context(), conn.ForgeProjectID, opts)
	if err != nil {
		h.forgeDriverError(w, err)
		return
	}
	truncated := false
	if len(issues) > MaxForgeListItems {
		truncated = true
		issues = issues[:MaxForgeListItems]
	}
	items := make([]apitypes.ForgeIssueSummaryDTO, 0, len(issues))
	for _, issue := range issues {
		items = append(items, apitypes.ForgeIssueSummaryDTO{
			IID:       issue.IID,
			Title:     issue.Title,
			State:     issue.State,
			Labels:    nonNilStrings(issue.Labels),
			Author:    issue.Author,
			UpdatedAt: rfc3339(issue.UpdatedAt),
		})
	}
	httpx.JSON(w, http.StatusOK, apitypes.ForgeIssueListDTO{
		Items:     items,
		Truncated: truncated,
		Returned:  len(items),
	})
}

// WorkerForgeListIssueLabelEvents reads one issue's label-event list from the run's
// forge, truncating the RESPONSE to MaxForgeListItems (the driver has no upstream
// Limit — see the cap block; the full per-issue set is fetched, then capped).
// GET /worker/runs/{id}/forge/issues/{iid}/label-events (PRD #158 M1).
func (h *Handler) WorkerForgeListIssueLabelEvents(w http.ResponseWriter, r *http.Request) {
	conn, f, ok := h.resolveForgeRun(w, r)
	if !ok {
		return
	}
	iid, err := parseInt64(chi.URLParam(r, "iid"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, forgeErrInvalid)
		return
	}
	events, err := f.ListIssueLabelEvents(r.Context(), conn.ForgeProjectID, iid)
	if err != nil {
		h.forgeDriverError(w, err)
		return
	}
	truncated := false
	if len(events) > MaxForgeListItems {
		truncated = true
		events = events[:MaxForgeListItems]
	}
	items := make([]apitypes.ForgeLabelEventDTO, 0, len(events))
	for _, e := range events {
		items = append(items, apitypes.ForgeLabelEventDTO{
			ID:        e.ID,
			Action:    e.Action,
			LabelName: e.LabelName,
			Username:  e.Username,
			CreatedAt: rfc3339(e.CreatedAt),
		})
	}
	httpx.JSON(w, http.StatusOK, apitypes.ForgeLabelEventListDTO{
		Items:     items,
		Truncated: truncated,
		Returned:  len(items),
	})
}

// WorkerForgeGetMergeRequest reads one merge request from the run's forge.
// GET /worker/runs/{id}/forge/merge-requests/{iid} (PRD #158 M1).
func (h *Handler) WorkerForgeGetMergeRequest(w http.ResponseWriter, r *http.Request) {
	conn, f, ok := h.resolveForgeRun(w, r)
	if !ok {
		return
	}
	iid, err := parseInt64(chi.URLParam(r, "iid"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, forgeErrInvalid)
		return
	}
	mrq, err := f.GetMergeRequest(r.Context(), conn.ForgeProjectID, iid)
	if err != nil {
		h.forgeDriverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.ForgeMergeRequestDTO{
		IID:   mrq.IID,
		State: mrq.State,
	})
}

// WorkerForgePipelineJobs reads one pipeline's job list from the run's forge,
// truncating the RESPONSE to MaxForgeListItems (the driver has no upstream Limit —
// see the cap block; the full per-pipeline set is fetched, then capped).
// GET /worker/runs/{id}/forge/pipelines/{pipeline_id}/jobs (PRD #158 M1).
func (h *Handler) WorkerForgePipelineJobs(w http.ResponseWriter, r *http.Request) {
	conn, f, ok := h.resolveForgeRun(w, r)
	if !ok {
		return
	}
	pipelineID, err := parseInt64(chi.URLParam(r, "pipeline_id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, forgeErrInvalid)
		return
	}
	jobs, err := f.ListPipelineJobs(r.Context(), conn.ForgeProjectID, pipelineID)
	if err != nil {
		h.forgeDriverError(w, err)
		return
	}
	truncated := false
	if len(jobs) > MaxForgeListItems {
		truncated = true
		jobs = jobs[:MaxForgeListItems]
	}
	items := make([]apitypes.ForgeJobDTO, 0, len(jobs))
	for _, j := range jobs {
		items = append(items, apitypes.ForgeJobDTO{
			ID:     j.ID,
			Name:   j.Name,
			Stage:  j.Stage,
			Status: j.Status,
		})
	}
	httpx.JSON(w, http.StatusOK, apitypes.ForgeJobListDTO{
		Items:     items,
		Truncated: truncated,
		Returned:  len(items),
	})
}

// WorkerForgeLatestPipeline reads the newest pipeline for a ref OR merge request from
// the run's forge. GET /worker/runs/{id}/forge/latest-pipeline?ref=... OR ?mr_iid=...
// EXACTLY ONE of ref/mr_iid is required (PRD #158 M1). A ref/MR that never ran CI is
// {"pipeline":null} with 200 — NOT an error, so the agent can tell CI-never-ran apart
// from forge-down.
func (h *Handler) WorkerForgeLatestPipeline(w http.ResponseWriter, r *http.Request) {
	conn, f, ok := h.resolveForgeRun(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	ref := q.Get("ref")
	mrRaw := q.Get("mr_iid")
	// Exactly one of ref / mr_iid — both-or-neither is a bad request.
	if (ref == "") == (mrRaw == "") {
		httpx.Error(w, http.StatusBadRequest, forgeErrInvalid)
		return
	}
	var pipe forge.Pipeline
	var err error
	if ref != "" {
		pipe, err = f.LatestPipeline(r.Context(), conn.ForgeProjectID, ref)
	} else {
		mrIID, perr := parseInt64(mrRaw)
		if perr != nil {
			httpx.Error(w, http.StatusBadRequest, forgeErrInvalid)
			return
		}
		pipe, err = f.LatestMRPipeline(r.Context(), conn.ForgeProjectID, mrIID)
	}
	if err != nil {
		if errors.Is(err, forge.ErrNoPipeline) {
			httpx.JSON(w, http.StatusOK, apitypes.ForgeLatestPipelineDTO{Pipeline: nil})
			return
		}
		h.forgeDriverError(w, err)
		return
	}
	dto := apitypes.ForgePipelineDTO{
		ID:        pipe.ID,
		Ref:       pipe.Ref,
		SHA:       pipe.SHA,
		Status:    pipe.Status,
		CreatedAt: rfc3339(pipe.CreatedAt),
		UpdatedAt: rfc3339(pipe.UpdatedAt),
	}
	httpx.JSON(w, http.StatusOK, apitypes.ForgeLatestPipelineDTO{Pipeline: &dto})
}

// truncateForgeBody caps s at MaxForgeBodyBytes without splitting a UTF-8 rune,
// returning the (possibly truncated) string and whether it was cut. It trims any
// partial trailing rune left by the byte-boundary slice.
func truncateForgeBody(s string) (string, bool) {
	if len(s) <= MaxForgeBodyBytes {
		return s, false
	}
	b := []byte(s)[:MaxForgeBodyBytes]
	for len(b) > 0 {
		if r, size := utf8.DecodeLastRune(b); r == utf8.RuneError && size <= 1 {
			b = b[:len(b)-1]
			continue
		}
		break
	}
	return string(b), true
}
