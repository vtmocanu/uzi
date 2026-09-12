package handler

// forgeview.go serves the read-only forge views (PRD #1255 M2a): open pulls (list +
// drill-in with checks/reviews/merge) and CI runs (list + drill-in with jobs/steps).
// The routes are owner-scoped (repoForRequest) AND enabled-gated (a disabled repo has
// no forge view, so it 404s like an unknown one), carry the per-user forge budget, and
// read the forge on-demand through the connection PAT — nothing is persisted (D4). In
// M2a the forge is called directly, with no memo/ETag (that is M2b). Every forge error
// is already PAT-redacted by the driver; a *forge.RateLimitError maps to 429 +
// Retry-After, ErrForgeVersionUnsupported to an honest empty state, other errors to 502.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pipelinestatus"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	// forgeViewPullsLimit caps the open-PR refs page the pulls list fetches, and with it
	// the per-PR summary fan-out (one GetMergeRequestSummary per ref; D4: cold-list cost
	// 1 + 2×min(PRs,30)). The pull DETAIL route no longer reuses it — it fetches one PR
	// by iid directly (GetMergeRequestSummary), so it finds ANY open PR, not only one on
	// this page.
	forgeViewPullsLimit = 30
	// forgeViewCIRunsDefault / forgeViewCIRunsMax bound the `ci` list page (?limit=).
	forgeViewCIRunsDefault = 30
	forgeViewCIRunsMax     = 100
	// forgeViewUnsupportedMsg is the honest empty-state sentence for a forge version
	// without the CI-runs endpoint (ErrForgeVersionUnsupported).
	forgeViewUnsupportedMsg = "CI runs are not available on this forge version"
)

// forgeViewRepo runs the owner-scope preflight AND the enabled gate shared by every
// forge-view route: repoForRequest 404s an unknown/foreign repo, and a disabled repo
// 404s here too (the shared helper does not gate enabled, but a disabled repo has no
// forge view). ok=false means a response was already written.
func (h *Handler) forgeViewRepo(w http.ResponseWriter, r *http.Request) (store.GetRepoForUserRow, bool) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return store.GetRepoForUserRow{}, false
	}
	if !repo.Enabled {
		httpx.Error(w, http.StatusNotFound, "repo not found")
		return store.GetRepoForUserRow{}, false
	}
	return repo, true
}

// writeForgeError maps a forge read error to an HTTP status. A *forge.RateLimitError
// (errors.As) becomes 429 with Retry-After (from its Retry duration or Reset wall
// time); any other error is 502 with the already-redacted message, matching ci_fix.go.
func (h *Handler) writeForgeError(w http.ResponseWriter, op string, err error) {
	var rl *forge.RateLimitError
	if errors.As(err, &rl) {
		if secs := forgeRetryAfterSeconds(rl); secs > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
		httpx.Error(w, http.StatusTooManyRequests, "the forge rate-limited this request; retry later")
		return
	}
	// err is already PAT-redacted by the driver.
	slog.Warn("forge view: "+op, "error", err)
	httpx.Error(w, http.StatusBadGateway, "forge request failed: "+err.Error())
}

// forgeRetryAfterSeconds derives a whole-second Retry-After hint from a rate-limit
// error, rounding up so a sub-second wait still asks for at least 1s. Zero when the
// forge supplied neither a retry duration nor a reset time.
func forgeRetryAfterSeconds(rl *forge.RateLimitError) int {
	var d time.Duration
	switch {
	case rl.Retry > 0:
		d = rl.Retry
	case !rl.Reset.IsZero():
		d = time.Until(rl.Reset)
	}
	if d <= 0 {
		return 0
	}
	secs := int(d / time.Second)
	if d%time.Second > 0 {
		secs++
	}
	return secs
}

// ListPulls serves GET /api/repos/{id}/pulls: the repo's open PRs as list rows, each
// linked to the newest uzi run on that (repo, mr_iid). It reads the cheap open-PR refs
// in one list call (ListMergeRequestRefs), then a per-iid GetMergeRequestSummary for
// each (a SEQUENTIAL loop for now — the pool-of-4 + memo is M2b part 2). It must NOT
// fan out a per-PR checks call (checks appear on the drill-in, D4). A PR that raced
// closed between the refs list and its summary fetch (ErrMergeRequestNotFound) is
// SKIPPED, not fatal — the list still returns the PRs that survived.
func (h *Handler) ListPulls(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.forgeViewRepo(w, r)
	if !ok {
		return
	}
	f, err := h.forgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("forge view: build forge", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	refs, err := f.ListMergeRequestRefs(r.Context(), repo.ForgeProjectID,
		forge.ListMergeRequestsOptions{State: forge.MRStateOpened, Limit: forgeViewPullsLimit})
	if err != nil {
		h.writeForgeError(w, "list pulls", err)
		return
	}
	pulls := make([]apitypes.PullDTO, 0, len(refs))
	for _, ref := range refs {
		s, err := f.GetMergeRequestSummary(r.Context(), repo.ForgeProjectID, ref.IID)
		if err != nil {
			if errors.Is(err, forge.ErrMergeRequestNotFound) {
				continue // raced closed between the list and the fetch — skip, don't fail the list
			}
			h.writeForgeError(w, "get pull summary", err)
			return
		}
		p := pullDTOFromSummary(s)
		p.RunID = h.newestRunIDForMR(r.Context(), repo.ID, ref.IID)
		pulls = append(pulls, p)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"pulls": pulls})
}

// GetPull serves GET /api/repos/{id}/pulls/{iid}: one open PR with its checks, reviews
// and derived merge state. It fetches the PR's summary directly by iid
// (GetMergeRequestSummary — head_sha, review decision, conflicts, diff stats, state),
// then reads ListChecks(head_sha) and ListMergeRequestReviews(iid). A PR the forge 404s
// (ErrMergeRequestNotFound) or one whose State is not opened (closed/merged/locked)
// 404s here too. This finds ANY open PR by iid (≤4 forge calls), not only one on the
// most-recently-updated page.
func (h *Handler) GetPull(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.forgeViewRepo(w, r)
	if !ok {
		return
	}
	iid, err := parseInt64(chi.URLParam(r, "iid"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid pull id")
		return
	}
	f, err := h.forgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("forge view: build forge", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	s, err := f.GetMergeRequestSummary(r.Context(), repo.ForgeProjectID, iid)
	if err != nil {
		if errors.Is(err, forge.ErrMergeRequestNotFound) {
			httpx.Error(w, http.StatusNotFound, "pull request not found")
			return
		}
		h.writeForgeError(w, "get pull", err)
		return
	}
	if s.State != forge.MRStateOpened {
		httpx.Error(w, http.StatusNotFound, "pull request not found")
		return
	}
	checks, err := f.ListChecks(r.Context(), repo.ForgeProjectID, s.HeadSHA)
	if err != nil {
		h.writeForgeError(w, "list checks", err)
		return
	}
	reviews, err := f.ListMergeRequestReviews(r.Context(), repo.ForgeProjectID, iid)
	if err != nil {
		h.writeForgeError(w, "list reviews", err)
		return
	}
	detail := apitypes.PullDetailDTO{
		PullDTO: pullDTOFromSummary(s),
		Checks:  checkDTOs(checks),
		Reviews: reviewDTOs(reviews),
		Merge:   mergeStateDTO(s, checks),
	}
	detail.RunID = h.newestRunIDForMR(r.Context(), repo.ID, iid)
	httpx.JSON(w, http.StatusOK, detail)
}

// ListCIRuns serves GET /api/repos/{id}/ci/runs?limit=: the repo's CI runs newest-first
// (default 30, capped at 100). On a forge version without the runs endpoint it answers
// 200 with an empty list and an "unsupported" sentence rather than 500 (R2).
func (h *Handler) ListCIRuns(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.forgeViewRepo(w, r)
	if !ok {
		return
	}
	limit := forgeViewCIRunsDefault
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > forgeViewCIRunsMax {
		limit = forgeViewCIRunsMax
	}
	f, err := h.forgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("forge view: build forge", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	runs, err := f.ListWorkflowRuns(r.Context(), repo.ForgeProjectID, forge.ListWorkflowRunsOptions{Limit: limit})
	if err != nil {
		if errors.Is(err, forge.ErrForgeVersionUnsupported) {
			httpx.JSON(w, http.StatusOK, map[string]any{"runs": []apitypes.CIRunDTO{}, "unsupported": forgeViewUnsupportedMsg})
			return
		}
		h.writeForgeError(w, "list ci runs", err)
		return
	}
	dtos := make([]apitypes.CIRunDTO, 0, len(runs))
	for _, run := range runs {
		dtos = append(dtos, ciRunDTO(run))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"runs": dtos, "unsupported": ""})
}

// GetCIRun serves GET /api/repos/{id}/ci/runs/{run_id}: the run's jobs and steps. The
// run-level scalars the drill-in header shows come from the list row the client drilled
// in from, so this route fills only id (from the path) and jobs. On a forge version
// without the endpoint it answers 200 with empty jobs and the "unsupported" sentence.
func (h *Handler) GetCIRun(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.forgeViewRepo(w, r)
	if !ok {
		return
	}
	runID, err := parseInt64(chi.URLParam(r, "run_id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	f, err := h.forgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("forge view: build forge", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	detail := apitypes.CIRunDetailDTO{CIRunDTO: apitypes.CIRunDTO{ID: runID}, Jobs: []apitypes.CIJobDTO{}}
	jobs, err := f.ListPipelineJobs(r.Context(), repo.ForgeProjectID, runID)
	if err != nil {
		if errors.Is(err, forge.ErrForgeVersionUnsupported) {
			detail.Unsupported = forgeViewUnsupportedMsg
			httpx.JSON(w, http.StatusOK, detail)
			return
		}
		h.writeForgeError(w, "ci run jobs", err)
		return
	}
	detail.Jobs = ciJobDTOs(jobs)
	httpx.JSON(w, http.StatusOK, detail)
}

// newestRunIDForMR returns the newest uzi run opened against (repoID, iid) as a string,
// or nil when none. A query error degrades to nil (the `↳ run` link simply does not
// appear) rather than failing the whole list — the run link is an enrichment, not the
// forge data itself.
func (h *Handler) newestRunIDForMR(ctx context.Context, repoID uuid.UUID, iid int64) *string {
	runs, err := h.q.ListRunsForMR(ctx, store.ListRunsForMRParams{RepoID: repoID, MrIid: pgconv.Int8Ptr(&iid)})
	if err != nil {
		slog.Warn("forge view: list runs for mr", "error", err)
		return nil
	}
	if len(runs) == 0 {
		return nil
	}
	id := runs[0].ID.String()
	return &id
}

// pullDTOFromSummary maps a neutral MergeRequestSummary to the wire PullDTO (RunID is
// filled by the caller from ListRunsForMR).
func pullDTOFromSummary(s forge.MergeRequestSummary) apitypes.PullDTO {
	return apitypes.PullDTO{
		IID:            s.IID,
		Title:          s.Title,
		Author:         s.Author,
		SourceBranch:   s.SourceBranch,
		TargetBranch:   s.TargetBranch,
		HeadSHA:        s.HeadSHA,
		Draft:          s.Draft,
		Conflicts:      s.Conflicts,
		ReviewDecision: string(s.ReviewDecision),
		WebURL:         s.WebURL,
		Additions:      s.Additions,
		Deletions:      s.Deletions,
		Commits:        s.Commits,
		CreatedAt:      s.CreatedAt,
		UpdatedAt:      s.UpdatedAt,
	}
}

// checkDTOs maps neutral checks to wire CheckDTOs, always non-nil (the TS type is
// never-null; the api normalizes an empty forge result to []).
func checkDTOs(cs []forge.Check) []apitypes.CheckDTO {
	out := make([]apitypes.CheckDTO, 0, len(cs))
	for _, c := range cs {
		out = append(out, apitypes.CheckDTO{
			Name:        c.Name,
			Status:      c.Status,
			Conclusion:  c.Conclusion,
			Description: c.Description,
			WebURL:      c.WebURL,
			StartedAt:   c.StartedAt,
			CompletedAt: c.CompletedAt,
			Source:      c.Source,
		})
	}
	return out
}

// reviewDTOs maps neutral reviews to wire PullReviewDTOs, always non-nil.
func reviewDTOs(rs []forge.Review) []apitypes.PullReviewDTO {
	out := make([]apitypes.PullReviewDTO, 0, len(rs))
	for _, rv := range rs {
		out = append(out, apitypes.PullReviewDTO{Author: rv.Author, State: rv.State, SubmittedAt: rv.SubmittedAt})
	}
	return out
}

// mergeStateDTO derives the PR's merge readiness from its conflicts tri-state, review
// decision and checks (D3). BlockedReason has a fixed priority: conflicts, then changes
// requested, then a failing check, then a pending check; "" when nothing blocks.
// RequiredChecksPassed is best-effort (no failing and no pending check) until
// branch-protection "required" detail lands (run 2). MergeableState is left "" — the
// neutral list summary does not expose the raw coarse forge state in M2a.
func mergeStateDTO(s forge.MergeRequestSummary, checks []forge.Check) apitypes.MergeStateDTO {
	anyFailing, anyPending := false, false
	for _, c := range checks {
		if checkFailing(c) {
			anyFailing = true
		} else if checkPending(c) {
			anyPending = true
		}
	}
	m := apitypes.MergeStateDTO{
		Conflicts:            s.Conflicts,
		RequiredChecksPassed: !anyFailing && !anyPending,
	}
	switch {
	case s.Conflicts != nil && *s.Conflicts:
		m.BlockedReason = "conflicts with main"
	case s.ReviewDecision == forge.ReviewChangesRequested:
		m.BlockedReason = "changes requested"
	case anyFailing:
		m.BlockedReason = "checks failing"
	case anyPending:
		m.BlockedReason = "waiting on checks"
	}
	return m
}

// checkFailing reports whether a check is a terminal failure — on its conclusion once
// completed, or on its status where the surface carries no conclusion split.
func checkFailing(c forge.Check) bool {
	return pipelinestatus.IsFailed(c.Conclusion) || pipelinestatus.IsFailed(c.Status)
}

// checkPending reports whether a check has not reached the completed phase yet.
func checkPending(c forge.Check) bool {
	return c.Status != "completed"
}

// ciRunDTO maps a neutral WorkflowRun to the wire CIRunDTO.
func ciRunDTO(r forge.WorkflowRun) apitypes.CIRunDTO {
	return apitypes.CIRunDTO{
		ID:         r.ID,
		Name:       r.Name,
		Number:     r.Number,
		Event:      r.Event,
		Branch:     r.Branch,
		SHA:        r.SHA,
		Status:     r.Status,
		Conclusion: r.Conclusion,
		Title:      r.Title,
		Actor:      r.Actor,
		WebURL:     r.WebURL,
		CreatedAt:  r.CreatedAt,
		UpdatedAt:  r.UpdatedAt,
		StartedAt:  r.StartedAt,
		JobsDone:   r.JobsDone,
		JobsTotal:  r.JobsTotal,
	}
}

// ciJobDTOs maps neutral jobs (with steps) to wire CIJobDTOs, always non-nil.
// Conclusion stays "" because the neutral forge.Job carries no status/conclusion split
// — every driver folds the terminal outcome into Status (githubActionsStatus for
// GitHub, the raw status for GitLab/Forgejo) — so the consumer classifies off Status
// with pipelinestatus.Tone, exactly as it does for a GitLab/Forgejo CIRun. Steps DO
// carry the split (forge.Step has both fields).
func ciJobDTOs(js []forge.Job) []apitypes.CIJobDTO {
	out := make([]apitypes.CIJobDTO, 0, len(js))
	for _, j := range js {
		out = append(out, apitypes.CIJobDTO{
			ID:         j.ID,
			Name:       j.Name,
			Status:     j.Status,
			Conclusion: "",
			WebURL:     j.WebURL,
			StartedAt:  j.StartedAt,
			FinishedAt: j.FinishedAt,
			Steps:      ciStepDTOs(j.Steps),
		})
	}
	return out
}

// ciStepDTOs maps neutral steps to wire CIStepDTOs, always non-nil (GitLab/Forgejo jobs
// carry no steps, so this is [] there).
func ciStepDTOs(ss []forge.Step) []apitypes.CIStepDTO {
	out := make([]apitypes.CIStepDTO, 0, len(ss))
	for _, s := range ss {
		out = append(out, apitypes.CIStepDTO{
			Name:        s.Name,
			Status:      s.Status,
			Conclusion:  s.Conclusion,
			Number:      s.Number,
			StartedAt:   s.StartedAt,
			CompletedAt: s.CompletedAt,
		})
	}
	return out
}
