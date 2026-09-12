package handler

// forgeview.go serves the read-only forge views (PRD #1255 M2a): open pulls (list +
// drill-in with checks/reviews/merge) and CI runs (list + drill-in with jobs/steps).
// The routes are owner-scoped (repoForRequest) AND enabled-gated (a disabled repo has
// no forge view, so it 404s like an unknown one), carry the per-user forge budget, and
// read the forge on-demand through the connection PAT — nothing is persisted (D4).
// Every forge read rides the tenant-scoped forgememo (M2b): a short-TTL + singleflight
// memo keyed by forgeMemoPrefix, so N terminals polling one route cost one forge
// round-trip per TTL window and an unchanged PR costs zero enrichment; the per-PR
// enrichment fans out through a bounded pool (forgeMemoFanout). The GitHub ETag
// transport and the per-connection outbound budget / rate-shedding are a later M2b
// unit — not here. Every forge error is already PAT-redacted by the driver; a
// *forge.RateLimitError maps to 429 + Retry-After (uncached through Do),
// ErrForgeVersionUnsupported to an honest empty state, other errors to 502.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

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

	// forgeMemoEntries / forgeMemoMaxBytes bound the process-wide forge-view read memo
	// (PRD #1255 D4): an LRU of 256 entries, with values over 512 KiB returned to the
	// caller but never stored. The forge-view DTOs are small, so the byte cap rarely
	// bites. Referenced by New (handler.go) and memo() (handler.go).
	forgeMemoEntries  = 256
	forgeMemoMaxBytes = 512 * 1024
	// forgeMemoTTL is the memo freshness window (D4): within it, N terminals polling a
	// route cost one forge round-trip and an unchanged PR costs zero enrichment calls.
	forgeMemoTTL = 5 * time.Second
	// forgeMemoFanout bounds the per-PR summary fan-out ListPulls runs, so a burst of
	// enrichment calls never trips a forge's secondary rate limit (D4: a pool of 4).
	forgeMemoFanout = 4
)

// forgeMemoPrefix builds the tenant-scoped key prefix every forge-view memo key starts
// with (PRD #1255 D4). It ISOLATES tenants: singleflight collapses concurrent misses on
// one key, so an under-scoped key would serve connection A's data to connection B. The
// prefix is the connection identity + repo: the connection_id, a sha256 of the sealed
// token ciphertext (so a rotated PAT — different ciphertext — misses naturally and two
// connections never share an entry), and the repo id (an iid like PR #5 exists on every
// repo, so no key may start below the repo). The RAW token never enters the key — only
// its hash. The trailing "|" keeps the per-route suffix from abutting the repo id.
func forgeMemoPrefix(repo store.GetRepoForUserRow) string {
	sum := sha256.Sum256(repo.TokenCiphertext)
	return fmt.Sprintf("%s|%x|%s|", repo.ConnectionID, sum, repo.ID)
}

// memoSize is the cheap byte-size estimate the memo's LRU/512-KiB ceiling uses: the
// length of the value's JSON encoding. The forge-view values are small structs, so an
// exact accounting is not worth the cost; a marshal failure (never expected for these
// plain DTO-shaped structs) reports 0 so the value is still cached as a tiny entry.
func memoSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

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
// each, fanned out through a bounded pool of forgeMemoFanout (D4). Both layers ride the
// tenant-scoped memo (M2b): the refs call is keyed on (state,limit) so N terminals cost
// one round-trip per TTL, and each per-PR summary is keyed on the ref's UpdatedAt — the
// CHANGE KEY — so an unchanged PR within the TTL window costs zero calls while a PR
// whose activity moved gets a fresh key and reloads. It must NOT fan out a per-PR checks
// call (checks appear on the drill-in, D4). A PR that raced closed between the refs list
// and its summary fetch (ErrMergeRequestNotFound) is SKIPPED, not fatal; any other
// per-PR error aborts the request (writeForgeError). The run_id linkage (newestRunIDForMR,
// a cheap local DB read) is deliberately NOT memoised.
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
	ctx := r.Context()
	// loadCtx detaches the memoized forge loads from the REQUEST's cancellation
	// (context.WithoutCancel, Go 1.21+): singleflight collapses N co-polling clients onto
	// ONE shared load, so if the leader disconnects mid-load its r.Context() cancellation
	// would otherwise surface as context.Canceled to a still-connected follower and 502 a
	// healthy request. The driver's own client Timeout still bounds each HTTP call, so
	// detaching does not remove the per-call deadline. The DB run-link read below stays on
	// ctx (r.Context()) — it is per-request, not singleflighted.
	loadCtx := context.WithoutCancel(r.Context())
	prefix := forgeMemoPrefix(repo)

	refsKey := prefix + "refs|" + forge.MRStateOpened + "|" + strconv.Itoa(forgeViewPullsLimit)
	refsV, err := h.memo().Do(refsKey, forgeMemoTTL, func() (any, int, error) {
		refs, e := f.ListMergeRequestRefs(loadCtx, repo.ForgeProjectID,
			forge.ListMergeRequestsOptions{State: forge.MRStateOpened, Limit: forgeViewPullsLimit})
		if e != nil {
			return nil, 0, e
		}
		return refs, memoSize(refs), nil
	})
	if err != nil {
		h.writeForgeError(w, "list pulls", err)
		return
	}
	refs := refsV.([]forge.MergeRequestRef)

	// Fan the per-PR enrichment out through a pool bounded at forgeMemoFanout. Each
	// goroutine writes only its own results[i], so the pre-sized, position-indexed slice
	// carries no data race (distinct elements) and preserves the input ref order; the
	// errgroup's zero value does NOT cancel on error, so g.Wait returns the first
	// non-sentinel error while every goroutine (and its memo entry) runs to completion (no
	// errgroup-induced cancellation of a sibling's shared load). Decoupling from the
	// REQUEST's cancellation is handled separately by loadCtx (context.WithoutCancel), so
	// a disconnecting client cannot cancel a singleflight load a follower is awaiting.
	type pullResult struct {
		dto  apitypes.PullDTO
		skip bool // raced closed (ErrMergeRequestNotFound) — omitted from the list
	}
	results := make([]pullResult, len(refs))
	var g errgroup.Group
	g.SetLimit(forgeMemoFanout)
	for i, ref := range refs {
		g.Go(func() error {
			sumKey := prefix + "sum|" + strconv.FormatInt(ref.IID, 10) + "|" + ref.UpdatedAt.UTC().Format(time.RFC3339Nano)
			v, e := h.memo().Do(sumKey, forgeMemoTTL, func() (any, int, error) {
				s, se := f.GetMergeRequestSummary(loadCtx, repo.ForgeProjectID, ref.IID)
				if se != nil {
					return nil, 0, se
				}
				return s, memoSize(s), nil
			})
			if e != nil {
				if errors.Is(e, forge.ErrMergeRequestNotFound) {
					results[i].skip = true
					return nil // raced closed between the list and the fetch — skip, don't fail the list
				}
				return e
			}
			p := pullDTOFromSummary(v.(forge.MergeRequestSummary))
			p.RunID = h.newestRunIDForMR(ctx, repo.ID, ref.IID)
			results[i].dto = p
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		h.writeForgeError(w, "get pull summary", err)
		return
	}
	pulls := make([]apitypes.PullDTO, 0, len(refs))
	for _, res := range results {
		if res.skip {
			continue
		}
		pulls = append(pulls, res.dto)
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
	ctx := r.Context()
	// loadCtx detaches the memoized forge reads from the request's cancellation so a
	// leader's mid-load disconnect cannot poison a co-polling follower's shared
	// singleflight load (see ListPulls). The DB run-link read below stays on ctx.
	loadCtx := context.WithoutCancel(r.Context())
	prefix := forgeMemoPrefix(repo)
	iidStr := strconv.FormatInt(iid, 10)

	// Each of the three forge reads is memoised separately under the tenant prefix so a
	// drill-in polled by several terminals collapses to one round-trip per read per TTL.
	// The summary key is iid-only (the detail view has no UpdatedAt change key — that is
	// the list route's job); the 404 / non-open guards run AFTER the fetch and propagate
	// uncached through Do (errors are never memoised).
	sumV, err := h.memo().Do(prefix+"pull|"+iidStr, forgeMemoTTL, func() (any, int, error) {
		s, e := f.GetMergeRequestSummary(loadCtx, repo.ForgeProjectID, iid)
		if e != nil {
			return nil, 0, e
		}
		return s, memoSize(s), nil
	})
	if err != nil {
		if errors.Is(err, forge.ErrMergeRequestNotFound) {
			httpx.Error(w, http.StatusNotFound, "pull request not found")
			return
		}
		h.writeForgeError(w, "get pull", err)
		return
	}
	s := sumV.(forge.MergeRequestSummary)
	if s.State != forge.MRStateOpened {
		httpx.Error(w, http.StatusNotFound, "pull request not found")
		return
	}
	checksV, err := h.memo().Do(prefix+"checks|"+s.HeadSHA, forgeMemoTTL, func() (any, int, error) {
		cs, e := f.ListChecks(loadCtx, repo.ForgeProjectID, s.HeadSHA)
		if e != nil {
			return nil, 0, e
		}
		return cs, memoSize(cs), nil
	})
	if err != nil {
		h.writeForgeError(w, "list checks", err)
		return
	}
	checks := checksV.([]forge.Check)
	reviewsV, err := h.memo().Do(prefix+"reviews|"+iidStr, forgeMemoTTL, func() (any, int, error) {
		rv, e := f.ListMergeRequestReviews(loadCtx, repo.ForgeProjectID, iid)
		if e != nil {
			return nil, 0, e
		}
		return rv, memoSize(rv), nil
	})
	if err != nil {
		h.writeForgeError(w, "list reviews", err)
		return
	}
	reviews := reviewsV.([]forge.Review)
	detail := apitypes.PullDetailDTO{
		PullDTO: pullDTOFromSummary(s),
		Checks:  checkDTOs(checks),
		Reviews: reviewDTOs(reviews),
		Merge:   mergeStateDTO(s, checks),
	}
	detail.RunID = h.newestRunIDForMR(ctx, repo.ID, iid)
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
	// loadCtx detaches the memoized forge read from the request's cancellation so a
	// leader's mid-load disconnect cannot poison a co-polling follower's shared
	// singleflight load (see ListPulls). This route has no per-request DB read.
	loadCtx := context.WithoutCancel(r.Context())
	runsKey := forgeMemoPrefix(repo) + "ciruns|" + strconv.Itoa(limit)
	runsV, err := h.memo().Do(runsKey, forgeMemoTTL, func() (any, int, error) {
		runs, e := f.ListWorkflowRuns(loadCtx, repo.ForgeProjectID, forge.ListWorkflowRunsOptions{Limit: limit})
		if e != nil {
			return nil, 0, e
		}
		return runs, memoSize(runs), nil
	})
	if err != nil {
		if errors.Is(err, forge.ErrForgeVersionUnsupported) {
			httpx.JSON(w, http.StatusOK, map[string]any{"runs": []apitypes.CIRunDTO{}, "unsupported": forgeViewUnsupportedMsg})
			return
		}
		h.writeForgeError(w, "list ci runs", err)
		return
	}
	runs := runsV.([]forge.WorkflowRun)
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
	// loadCtx detaches the memoized forge read from the request's cancellation so a
	// leader's mid-load disconnect cannot poison a co-polling follower's shared
	// singleflight load (see ListPulls). This route has no per-request DB read.
	loadCtx := context.WithoutCancel(r.Context())
	detail := apitypes.CIRunDetailDTO{CIRunDTO: apitypes.CIRunDTO{ID: runID}, Jobs: []apitypes.CIJobDTO{}}
	jobsKey := forgeMemoPrefix(repo) + "cijobs|" + strconv.FormatInt(runID, 10)
	jobsV, err := h.memo().Do(jobsKey, forgeMemoTTL, func() (any, int, error) {
		jobs, e := f.ListPipelineJobs(loadCtx, repo.ForgeProjectID, runID)
		if e != nil {
			return nil, 0, e
		}
		return jobs, memoSize(jobs), nil
	})
	if err != nil {
		if errors.Is(err, forge.ErrForgeVersionUnsupported) {
			detail.Unsupported = forgeViewUnsupportedMsg
			httpx.JSON(w, http.StatusOK, detail)
			return
		}
		h.writeForgeError(w, "ci run jobs", err)
		return
	}
	detail.Jobs = ciJobDTOs(jobsV.([]forge.Job))
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
