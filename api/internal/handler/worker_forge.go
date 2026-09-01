package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

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
	// forgeErrMRThreadScope is the fixed, coordinate-free rejection for the Decision-11
	// scope check on the MR-thread write endpoints: the supplied reply/resolve id is not
	// a thread in THIS run's review snapshot (or the run carries no MR to write to). It
	// is the server-side guard that makes an injected "resolve all open threads" a no-op.
	forgeErrMRThreadScope = "thread is not part of this run's review"
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
	runID, ok := httpx.PathUUIDMsg(w, r, "id", forgeErrInvalid)
	if !ok {
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
	// Fetch the issue's comments best-effort: a comments read failure must NOT fail
	// the whole get_issue (D3 freshness is a bonus over the description). Log the
	// (already PAT-redacted) driver error and return an empty comment list so the
	// description still reaches the agent.
	var comments []apitypes.ForgeIssueCommentDTO
	var commentsTruncated bool
	if raw, cerr := f.ListIssueComments(r.Context(), conn.ForgeProjectID, iid); cerr != nil {
		slog.Error("worker forge list issue comments", "error", cerr) // already PAT-redacted by the driver
	} else {
		comments, commentsTruncated = assembleForgeIssueComments(raw, conn.BotForgeUserID)
	}
	httpx.JSON(w, http.StatusOK, apitypes.ForgeIssueDTO{
		IID:                  issue.IID,
		Title:                issue.Title,
		State:                issue.State,
		Labels:               nonNilStrings(issue.Labels),
		Author:               issue.Author,
		UpdatedAt:            rfc3339(issue.UpdatedAt),
		Description:          desc,
		DescriptionTruncated: truncated,
		Comments:             nonNilForgeComments(comments),
		CommentsTruncated:    commentsTruncated,
	})
}

// maxForgeCommentItems bounds the number of comments the single-issue GET returns,
// independent of the byte cap — a flood of tiny comments must not blow up the payload.
const maxForgeCommentItems = 200

// nonNilForgeComments returns c, or an empty non-nil slice when c is nil, so the
// comments field marshals as [] rather than null (matching the nonNilStrings
// convention).
func nonNilForgeComments(c []apitypes.ForgeIssueCommentDTO) []apitypes.ForgeIssueCommentDTO {
	if c == nil {
		return []apitypes.ForgeIssueCommentDTO{}
	}
	return c
}

// assembleForgeIssueComments applies the get_issue route's OWN bound to the driver's
// complete, oldest-first, system-free comment list: D1 drop uzi's own bot comments
// (author id == botForgeUserID), D9 fail-safe (botForgeUserID==0 ⇒ return none rather
// than risk leaking uzi's own comments), then keep the NEWEST maxForgeCommentItems and
// cap total body bytes at MaxForgeBodyBytes over that tail, setting truncated when it
// clips. Output stays oldest-first. Mirrors workersvc's M2b cap semantics but is kept
// self-contained in the handler package (no workersvc import).
func assembleForgeIssueComments(in []forge.IssueComment, botForgeUserID int64) ([]apitypes.ForgeIssueCommentDTO, bool) {
	// D9 fail-safe: an unknown/zero bot id cannot power the D1 self-filter, so omit
	// comments entirely rather than risk exposing uzi's own comments.
	if botForgeUserID == 0 {
		return nil, false
	}

	// D1 self-filter: drop every comment uzi's own bot authored, preserving order.
	kept := make([]forge.IssueComment, 0, len(in))
	for _, c := range in {
		if c.AuthorForgeUserID == botForgeUserID {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return nil, false
	}

	truncated := false

	// Count cap over the NEWEST tail, applied before the byte cap so a flood of tiny
	// comments cannot retain an unbounded number of entries (metadata amplification).
	if len(kept) > maxForgeCommentItems {
		kept = kept[len(kept)-maxForgeCommentItems:]
		truncated = true
	}

	// Byte cap over the NEWEST tail. Sum the kept bodies; if they fit, keep all.
	total := 0
	for _, c := range kept {
		total += len(c.Body)
	}
	if total > MaxForgeBodyBytes {
		truncated = true
		// Walk from the newest (last) backward, accumulating until adding the
		// next-older body would exceed the cap; the retained window is [start:].
		// Always keep at least the single newest comment.
		start := len(kept) - 1
		sum := len(kept[start].Body)
		for i := len(kept) - 2; i >= 0; i-- {
			if sum+len(kept[i].Body) > MaxForgeBodyBytes {
				break
			}
			sum += len(kept[i].Body)
			start = i
		}
		kept = kept[start:]
		// If the single newest comment's body alone exceeds the cap, truncate it
		// byte-safe on a UTF-8 rune boundary (mirroring truncateForgeBody).
		if len(kept) == 1 && len(kept[0].Body) > MaxForgeBodyBytes {
			kept[0].Body, _ = truncateForgeBody(kept[0].Body)
		}
	}

	out := make([]apitypes.ForgeIssueCommentDTO, 0, len(kept))
	for _, c := range kept {
		out = append(out, apitypes.ForgeIssueCommentDTO{
			Author:    c.AuthorUsername,
			CreatedAt: rfc3339(c.CreatedAt),
			Body:      c.Body,
		})
	}
	return out, truncated
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

// mrReworkThreadScope resolves the run's MR review snapshot and mr_iid for the Decision-11
// scope check on the reply/resolve write endpoints (PRD #700 M4). Both come from the SAME
// worker-owned run read resolveForgeRun already performed (ForgeConnForRun carries them on
// ForgeConn), so there is no second, unscoped run read. It writes the response and returns
// ok=false when the run carries no mr_iid (it is not an mr_rework run with a source MR to
// write to → 422) or the snapshot JSON is corrupt.
func (h *Handler) mrReworkThreadScope(w http.ResponseWriter, conn workersvc.ForgeConn) (workersvc.ReviewCommentsSnapshot, int64, bool) {
	if conn.MRIID == nil {
		// No source MR on this run → there is nothing this run may write back to.
		httpx.Error(w, http.StatusUnprocessableEntity, forgeErrMRThreadScope)
		return workersvc.ReviewCommentsSnapshot{}, 0, false
	}
	var snap workersvc.ReviewCommentsSnapshot
	if len(conn.ReviewComments) > 0 {
		if err := json.Unmarshal(conn.ReviewComments, &snap); err != nil {
			slog.Error("worker forge mr thread unmarshal snapshot", "error", err)
			httpx.Error(w, http.StatusBadGateway, forgeErrUpstream)
			return workersvc.ReviewCommentsSnapshot{}, 0, false
		}
	}
	// An absent/empty snapshot leaves snap.Comments nil, so every scope check below fails
	// closed: a run with no review snapshot can write back to nothing.
	return snap, *conn.MRIID, true
}

// snapshotHasReplyID reports whether replyID matches a thread present in the run's review
// snapshot (Decision 11). An empty ReplyID never matches, so a non-repliable comment
// cannot be used as an anchor.
func snapshotHasReplyID(snap workersvc.ReviewCommentsSnapshot, replyID string) bool {
	for _, c := range snap.Comments {
		if c.ReplyID != "" && c.ReplyID == replyID {
			return true
		}
	}
	return false
}

// snapshotHasResolveID reports whether resolveID matches a thread present in the run's
// review snapshot (Decision 11). An empty ResolveID never matches, so a non-resolvable
// comment (e.g. every Forgejo comment) cannot be used as an anchor.
func snapshotHasResolveID(snap workersvc.ReviewCommentsSnapshot, resolveID string) bool {
	for _, c := range snap.Comments {
		if c.ResolveID != "" && c.ResolveID == resolveID {
			return true
		}
	}
	return false
}

// WorkerForgeReplyMRThread posts a reply in an MR review thread the mr_rework run
// addressed. POST /worker/runs/{id}/forge/mr-threads/reply (PRD #700 M4, Decision 11).
// The endpoint derives the mr_iid from the OWNED run and rejects any reply_id not present
// in THIS run's review snapshot for THIS run's mr_iid — so an injected "reply/resolve
// everything" instruction can only ever act on threads that were genuinely part of this
// MR's review.
func (h *Handler) WorkerForgeReplyMRThread(w http.ResponseWriter, r *http.Request) {
	conn, f, ok := h.resolveForgeRun(w, r)
	if !ok {
		return
	}
	var req apitypes.ForgeMRThreadReplyRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, forgeErrInvalid)
		return
	}
	if strings.TrimSpace(req.ReplyID) == "" || strings.TrimSpace(req.Body) == "" {
		httpx.Error(w, http.StatusBadRequest, forgeErrInvalid)
		return
	}
	snap, mrIID, ok := h.mrReworkThreadScope(w, conn)
	if !ok {
		return
	}
	if !snapshotHasReplyID(snap, req.ReplyID) {
		httpx.Error(w, http.StatusForbidden, forgeErrMRThreadScope)
		return
	}
	if err := f.ReplyMergeRequestComment(r.Context(), conn.ForgeProjectID, mrIID, req.ReplyID, req.Body); err != nil {
		h.forgeDriverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.ForgeMRThreadReplyDTO{Replied: true})
}

// WorkerForgeResolveMRThread resolves an MR review thread the mr_rework run addressed.
// POST /worker/runs/{id}/forge/mr-threads/resolve (PRD #700 M4, Decision 11). Same scope
// check as reply. A driver that cannot resolve (Forgejo → forge.ErrResolveUnsupported) is
// TOLERATED: the endpoint returns 200 with resolved=false so the worker's reply still
// stands and the run does not fail (reply-only is the documented Forgejo contract).
func (h *Handler) WorkerForgeResolveMRThread(w http.ResponseWriter, r *http.Request) {
	conn, f, ok := h.resolveForgeRun(w, r)
	if !ok {
		return
	}
	var req apitypes.ForgeMRThreadResolveRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, forgeErrInvalid)
		return
	}
	if strings.TrimSpace(req.ResolveID) == "" {
		httpx.Error(w, http.StatusBadRequest, forgeErrInvalid)
		return
	}
	snap, mrIID, ok := h.mrReworkThreadScope(w, conn)
	if !ok {
		return
	}
	if !snapshotHasResolveID(snap, req.ResolveID) {
		httpx.Error(w, http.StatusForbidden, forgeErrMRThreadScope)
		return
	}
	err := f.ResolveMergeRequestThread(r.Context(), conn.ForgeProjectID, mrIID, req.ResolveID)
	if errors.Is(err, forge.ErrResolveUnsupported) {
		// Forgejo has no resolvable-thread concept: reply-only is the documented contract,
		// so a resolve is a tolerated no-op rather than a run failure.
		httpx.JSON(w, http.StatusOK, apitypes.ForgeMRThreadResolveDTO{Resolved: false})
		return
	}
	if err != nil {
		h.forgeDriverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.ForgeMRThreadResolveDTO{Resolved: true})
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
