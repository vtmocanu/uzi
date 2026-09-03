package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// runs_lifecycle.go holds the run create/dispatch/get/messages/input HTTP handlers
// and the guard-role-excluded notification glue (PRD #1022 file split).

// runInputKinds is the accepted steering-input set (mirrors the DB CHECK).
var runInputKinds = map[string]bool{
	"follow_up": true, "approve_plan": true, "reject_plan": true, "cancel": true, "revise_plan": true,
	"answer": true, "stop": true, "scope": true, // PRD #634 M2: accept a scope directive (CLI verb is m5)
}

// -------------------------------------------------------------------------
// Runs (session-authenticated)
// -------------------------------------------------------------------------

// CreateRun queues an agent run from a card. The issue must be a cached PRD
// issue with a PRD link in a repo the user owns. The issue description is
// snapshotted from the forge (the source of truth) at queue time — the board
// never stores descriptions, so the browser has none to send, and fetching it
// here keeps the run self-contained and authoritative.
func (h *Handler) CreateRun(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	user, _ := mw.UserFromContext(r.Context())
	var req struct {
		IssueIID int64 `json:"issue_iid"`
		// PRD #35 Decision 7: the per-run usage-limit opt-in, for CLI and API callers.
		// A POINTER, so "the caller said false" and "the caller said nothing" stay
		// distinct — a plain bool would make every existing client silently override
		// the user's own Settings default with false the moment this field shipped.
		// The web start button omits it and inherits the default (the user ruled
		// against a start-run modal on 2026-07-27); the per-run toggle is
		// PUT /api/runs/{id}/wait-on-limit.
		WaitOnLimit *bool `json:"wait_on_limit"`
		// PRD #841 M2: the per-run MR-rework override for CLI and API callers. A POINTER
		// so "the caller said false" and "the caller said nothing" stay distinct — nil
		// leaves the run inheriting the owner default live (D1), true/false stamp an
		// explicit override. The web start button omits it; the per-run toggle is
		// PUT /api/runs/{id}/mr-rework.
		MrReworkEnabled *bool `json:"mr_rework_enabled"`
		// PRD #209 seeded plan: an externally-authored plan and its optional agent
		// roster. A POINTER so absence (an ordinary run planned from the issue) stays
		// distinct from an empty string (a blank plan, which createRun rejects 422). The
		// web board start button omits both; the CLI `uzi run create --plan-file` sets
		// them (M3).
		PlanMd         *string                   `json:"plan_md"`
		AgentSelection *workersvc.AgentSelection `json:"agent_selection"`
		// PRD #209 M4 staleness guard: the commit the plan was written against, and
		// whether a divergence should fail the run rather than warn. PlannedCommit is a
		// POINTER so its absence is distinct from an empty string. Both are meaningful
		// ONLY alongside a plan_md (they describe a seeded run), and RequireBase is
		// meaningful only alongside a PlannedCommit — rejected below otherwise. The web
		// board omits both; the CLI `uzi run create --planned-commit/--require-base` sets
		// them.
		PlannedCommit *string `json:"planned_commit"`
		RequireBase   bool    `json:"require_base"`
		// Force (issue #856): bypass ONLY the create-time open-MR dedup (a completed prior
		// run still owning an OPEN MR for this issue). A plain bool — absence means "do not
		// force" — and it never affects the active-run gate. The CLI `uzi run create --force`
		// sets it (m2); the web board start button omits it.
		Force bool `json:"force"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.IssueIID <= 0 {
		httpx.Error(w, http.StatusBadRequest, "issue_iid must be a positive integer")
		return
	}

	// PRD #209: assemble the optional seeded plan. agent_selection is only meaningful
	// alongside a plan (it is the roster for the seeded implement run); a selection
	// with no plan is a confused request — the run would plan and gate normally, where
	// the selection is made anyway — so reject it here rather than persist a pointless
	// pre-gate selection. The plan itself is capped/scrubbed/empty-checked in CreateRun.
	var plannedCommit string
	if req.PlannedCommit != nil {
		plannedCommit = *req.PlannedCommit
	}
	var seed *workersvc.SeededPlan
	if req.PlanMd != nil {
		seed = &workersvc.SeededPlan{
			PlanMD:    *req.PlanMd,
			Selection: req.AgentSelection,
			// PRD #209 M4: the planned-against commit and the fail-on-divergence toggle.
			// Both ride only with a plan_md (rejected below otherwise).
			PlannedCommit: plannedCommit,
			RequireBase:   req.RequireBase,
		}
	} else if req.AgentSelection != nil {
		httpx.Error(w, http.StatusBadRequest, "agent_selection requires plan_md")
		return
	} else if req.PlannedCommit != nil || req.RequireBase {
		// PRD #209 M4: a planned_commit / require_base with no plan_md is a confused
		// request — the staleness guard describes a seeded run, and there is none here.
		// Rejected consistently with "agent_selection requires plan_md" above.
		httpx.Error(w, http.StatusBadRequest, "planned_commit and require_base require plan_md")
		return
	}
	// require_base is meaningful only with a planned_commit to compare against — without
	// one the flag can never fire. Reject it rather than silently persist a guard that is
	// inert by construction (the CLI raises the same usage error before it gets here).
	if seed != nil && req.RequireBase && strings.TrimSpace(seed.PlannedCommit) == "" {
		httpx.Error(w, http.StatusBadRequest, "require_base requires planned_commit")
		return
	}

	// The forge GetIssue snapshot, the uzi-label eligibility gate and the description
	// cap all live inside StartRunForUser (PRD #191 M1), shared with the Slack/web chat
	// start-run card.
	run, err := h.wsvc.StartRunForUser(r.Context(), user.ID, repo.ID, req.IssueIID, req.WaitOnLimit, req.MrReworkEnabled, req.Force, seed)
	if err != nil {
		h.writeStartRunError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run))})
}

// CreateTaskRunRequest is the POST /repos/{id}/task-runs body (PRD #400): the inline
// handoff context (reused as the run's issue_description), an optional source ref to
// branch from, and whether the worker should open an MR at the end. Context is
// required; the other two default to "branch from local HEAD" and "no MR".
type CreateTaskRunRequest struct {
	Context    string `json:"context"`
	BaseBranch string `json:"base_branch"`
	OpenMr     bool   `json:"open_mr"`
	// Interactive asks that the worker keep the run alive after signal_done (--interactive,
	// PRD #517 M1), parking it in awaiting_followup to iterate conversationally rather than
	// terminating; wound down with 'uzi run stop'. Defaults false (a plain handoff).
	Interactive bool `json:"interactive"`
	// ReviewRequested asks that a diff-review run be auto-created when this task completes
	// (--review, PRD #400 M4a): the review clones the finished branch, diffs it, and posts
	// structured findings the CLI fetches. Defaults false (a plain handoff).
	ReviewRequested bool `json:"review_requested"`
	// ThenFixRequested asks that, after the auto-review completes with findings, a chained
	// fix run push fixes to the same branch (--then-fix, PRD #400 M5). It implies a review;
	// the CLI sends both flags. Defaults false.
	ThenFixRequested bool `json:"then_fix_requested"`
}

// CreateTaskRun queues a task/handoff run (PRD #400). Unlike CreateRun it takes no
// forge issue: a task is issue-less and repo-ful, carrying its inline instruction
// directly. The server names the branch (uzi/task/<run-id>) and the created-run
// response carries it (runToDTO maps Branch), which is exactly what the CLI needs to
// push local HEAD to the seeded branch.
func (h *Handler) CreateTaskRun(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	user, _ := mw.UserFromContext(r.Context())
	var req CreateTaskRunRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Context) == "" {
		httpx.Error(w, http.StatusBadRequest, "context is required")
		return
	}
	run, err := h.wsvc.CreateTaskRun(r.Context(), user.ID, repo.ID, req.Context, req.BaseBranch, req.OpenMr, req.ReviewRequested, req.ThenFixRequested, req.Interactive)
	if err != nil {
		h.writeStartRunError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run))})
}

// DispatchTaskRun stamps a task run's dispatch gate (PRD #400 Decision 6): the CLI
// calls this AFTER it has pushed local HEAD to the run's uzi/task/<id> branch, which is
// the moment the run becomes claimable (ClaimRun gates task claimability on
// dispatched_at). Owner-scoped in the service; a run the caller does not own, a
// non-task run, or an already-dispatched run all map to 404 rather than confirming the
// run exists or re-broadcasting a claimable signal.
func (h *Handler) DispatchTaskRun(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	run, err := h.wsvc.DispatchTaskRun(r.Context(), user.ID, id)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("dispatch task run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run))})
}

// writeStartRunError maps the StartRunForUser* sentinels to an HTTP status + message.
// Shared by the board start button (CreateRun) and the chat start-run card
// (StartChatRun, PRD #191 M5) so both surfaces refuse an issue for the SAME reason with
// the SAME words — the PRD-gate hint especially, which names the instance's own labels.
func (h *Handler) writeStartRunError(w http.ResponseWriter, r *http.Request, err error) {
	// #66 D1 layer 2: the service-layer guardrail refused. 422 with the forge.go:191
	// body shape (error + violations) so the existing web 422 handling applies. Checked
	// before the switch because it carries a typed payload (the block-finding messages),
	// not just a status.
	var ge *workersvc.GuardrailBlockedError
	if errors.As(err, &ge) {
		httpx.JSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":      "this run was refused: the bot can push or merge to the repo's default branch, or that could not be verified (main is never touched). Fix branch protection on the forge, then retry.",
			"violations": ge.Findings,
		})
		return
	}
	switch {
	case errors.Is(err, workersvc.ErrForgeBuild):
		slog.Error("start run: build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	case errors.Is(err, workersvc.ErrForgeIssueRead):
		// err wraps the driver's already-redacted message.
		httpx.Error(w, http.StatusBadGateway, err.Error())
	case errors.Is(err, workersvc.ErrRepoNotFound):
		httpx.Error(w, http.StatusNotFound, "repo not found")
	case errors.Is(err, workersvc.ErrIssueNotFound):
		httpx.Error(w, http.StatusNotFound, "issue not found on this repo's board")
	case errors.Is(err, workersvc.ErrDescriptionTooLarge):
		httpx.Error(w, http.StatusUnprocessableEntity, "issue description is too large to run")
	case errors.Is(err, workersvc.ErrPlanTooLarge):
		httpx.Error(w, http.StatusUnprocessableEntity, "seeded plan is too large to run")
	case errors.Is(err, workersvc.ErrPlanEmpty):
		httpx.Error(w, http.StatusUnprocessableEntity, "seeded plan is empty")
	case errors.Is(err, workersvc.ErrPlanUnsafe):
		// issue #280: a seeded plan naming a bright-line recon target is refused
		// the ungated seeded fast-path. err.Error() carries the matched category
		// (a fixed planpolicy string, never plan text or a secret). Redirect to
		// the ordinary run flow, where the plan is reviewed at the approval gate.
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error()+
			"; create it as an ordinary run so the plan is reviewed at the approval gate")
	case errors.Is(err, workersvc.ErrInvalidPlannedCommit):
		httpx.Error(w, http.StatusBadRequest, "planned_commit must be a hex commit sha of 7-64 characters")
	case errors.Is(err, workersvc.ErrInvalidSelection):
		httpx.Error(w, http.StatusBadRequest, "invalid agent selection: "+err.Error())
	case errors.Is(err, workersvc.ErrNotPRDIssue):
		// PRD #764, widened by PRD #767: the single run-eligibility gate. An issue is
		// uzi's to run if it carries the uzi_label OR is assigned to the uzi-bot account
		// (assignment is the second eligibility signal). Tell the user either way to make
		// it eligible. A PRD link is no longer required, so this is the only eligibility
		// refusal.
		uziLabel, _ := h.settings.UziLabel(r.Context())
		httpx.Error(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("this issue is not marked as uzi's work; add the %s label or assign it to uzi, then start the run", uziLabel))
	case errors.Is(err, workersvc.ErrTaskBaseBranchTooLong):
		// PRD #400: the optional base_branch exceeded its dedicated cap. A caller error
		// (a git ref cannot legitimately be this long) → 400.
		httpx.Error(w, http.StatusBadRequest, "base branch is too long")
	case errors.Is(err, workersvc.ErrTaskBranchUnsafe):
		// PRD #400 Decision 8: the create-time namespace/default-branch assertion
		// failed. The server names the branch uzi/task/<uuid> itself, so this is an
		// internal invariant violation, never a caller error — 500, logged.
		slog.Error("create task run: branch-safety assertion failed", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	case errors.Is(err, workersvc.ErrOpenMRExists):
		// issue #856: distinct from the active-run 409. A machine-readable `code` +
		// structured `mr_iid` let the web compose its own confirm; `error` keeps the
		// full message (incl. the --force hint the CLI surfaces).
		body := map[string]any{"error": err.Error(), "code": "issue_has_open_mr"}
		var omErr *workersvc.OpenMRExistsError
		if errors.As(err, &omErr) {
			body["mr_iid"] = omErr.MRIID
		}
		httpx.JSON(w, http.StatusConflict, body)
	case errors.Is(err, workersvc.ErrActiveRunExists):
		httpx.Error(w, http.StatusConflict, "a run is already in progress for this issue")
	case errors.Is(err, workersvc.ErrBranchInUse):
		httpx.Error(w, http.StatusConflict, "a CI-fix run is already working this issue's branch; cancel it before starting an issue run")
	default:
		slog.Error("start run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

// GetRun returns one run visible to the viewer (owner, or any run for an admin).
func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	run, err := h.wsvc.GetRunForViewer(r.Context(), user.ID, user.IsAdmin, id)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("get run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	dto := runToDTO(run, h.runPriorityClass(r.Context(), run))
	// PRD #1064 M2: the server-derived "now" line. runToDTO stays pure, so the field is
	// set here in the caller from the batched lookup (one run this time). null for a
	// terminal run (no "now") and, via the batched query, for a run with no tool_use
	// frame. Best-effort: a lookup error leaves current_activity null rather than failing
	// the read of an otherwise-fine run.
	if !apitypes.IsTerminalRunStatus(run.Status) {
		if activity, err := h.wsvc.CurrentActivityForRuns(r.Context(), []uuid.UUID{run.ID}); err != nil {
			slog.Error("current activity", "run_id", run.ID, "error", err)
		} else {
			dto.CurrentActivity = activity[run.ID]
		}
	}
	// PRD #65 D2: stamp the run's forge for the run-view MR/PR noun. Best-effort and
	// only for a repo-ful run (chat runs have no repo, hence no MR affordance): a
	// lookup error leaves forge_type "" (the web defaults to GitLab's noun), never
	// failing the read of an otherwise-fine run.
	if run.RepoID.Valid {
		if ft, err := h.q.GetForgeTypeForRepo(r.Context(), uuid.UUID(run.RepoID.Bytes)); err != nil {
			slog.Error("resolve run forge type", "run_id", run.ID, "error", err)
		} else {
			dto.ForgeType = ft
		}
	}
	// PRD #411: stamp the run's originating forge issue web URL for the run-view #<iid>
	// link, resolved best-effort from the cached issues row — a join on GetRunByID* would
	// flip its return type and ripple through ~15 callers (Design Decision 2). Guarded on
	// BOTH a repo AND an issue: issue-less runs (task/ci_fix/prompt/chat) carry a NULL
	// issue_iid. A lookup miss (issue no longer cached) leaves issue_web_url nil, never
	// failing the read of an otherwise-fine run.
	if run.RepoID.Valid && run.IssueIid.Valid {
		if issue, err := h.q.GetIssueByIID(r.Context(), store.GetIssueByIIDParams{
			RepoID:        uuid.UUID(run.RepoID.Bytes),
			ForgeIssueIid: run.IssueIid.Int64,
		}); err != nil {
			slog.Debug("resolve run issue web url", "run_id", run.ID, "error", err)
		} else {
			webURL := issue.WebUrl
			dto.IssueWebURL = &webURL
		}
	}
	// PRD #37 M4-fix: resolve the owner's OWN-source roster here, on the detail read,
	// so the plan-gate picker sources its "My agent templates" chips from exactly the
	// roster the approve validator + worker use (allocation-resolved, lead stripped).
	// Best-effort: a lookup error leaves own_agents null (the picker degrades to no own
	// chips) rather than failing the read of an otherwise-fine run.
	if own, err := h.wsvc.OwnAgentRoster(r.Context(), run.UserID); err != nil {
		slog.Error("resolve own agent roster", "run_id", run.ID, "error", err)
	} else {
		dto.OwnAgents = own
	}
	// Attach the run's usage totals (PRD #40). No row → no usage: leave dto.Usage nil
	// (a pre-feature run shows nothing). Best-effort like OwnAgents above: a lookup
	// error must not fail the read of an otherwise-fine run.
	if u, err := h.wsvc.RunUsageTotal(r.Context(), run.ID); err == nil {
		dto.Usage = &apitypes.UsageDTO{
			InputTokens:         u.InputTokens,
			CacheReadTokens:     u.CacheReadTokens,
			CacheCreationTokens: u.CacheCreationTokens,
			OutputTokens:        u.OutputTokens,
			CostUSD:             numericToFloat(u.CostUsd),
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("get run usage total", "run_id", run.ID, "error", err)
	}
	// issue #403 F1/F6: stamp the branch-wide `uzi handoff rm` preconditions for a task run so
	// the CLI can refuse rm when the branch still has a live run (a running original or an
	// in-flight review/fix child) or belongs to a task that opened an MR. Owner-scoped query.
	// FAIL-CLOSED: on a lookup error leave BranchHasActiveRun=true so rm refuses rather than
	// deletes a branch under uncertainty.
	if run.Kind == runkind.Task && run.Branch.Valid && run.Branch.String != "" {
		if stats, err := h.q.TaskBranchRmStats(r.Context(), store.TaskBranchRmStatsParams{
			UserID: run.UserID,
			Branch: run.Branch,
		}); err != nil {
			slog.Error("task branch rm stats", "run_id", run.ID, "error", err)
			dto.BranchHasActiveRun = true // fail closed
		} else {
			dto.BranchHasActiveRun = stats.ActiveCount > 0
			dto.BranchHasOpenMr = stats.MrCount > 0
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": dto})
}

// maxRunMessagesPage caps the ?limit= page size on ListRunMessages so a single
// bounded response can never be unbounded; a caller asking for more is clamped
// down to this. Unset limit keeps the legacy unbounded path (non-breaking).
const maxRunMessagesPage = 1000

// ListRunMessages returns a run's persisted messages after ?after=<seq> (default
// 0), the replay source a reconnecting browser reads before going live. Visible
// to the run's owner or an admin. With ?limit=<n> (n >= 1) it returns a bounded
// page of at most n (clamped to maxRunMessagesPage) for the CLI's paging; absent
// limit is the unbounded legacy path the web SPA relies on.
func (h *Handler) ListRunMessages(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	after := int32(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n < 0 {
			httpx.Error(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = int32(n)
	}
	limit := int32(0)
	bounded := false
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n < 1 {
			httpx.Error(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > maxRunMessagesPage {
			n = maxRunMessagesPage
		}
		limit = int32(n)
		bounded = true
	}
	var msgs []store.RunMessage
	var err error
	if bounded {
		msgs, err = h.wsvc.ListRunMessagesForViewerPage(r.Context(), user.ID, user.IsAdmin, id, after, limit)
	} else {
		msgs, err = h.wsvc.ListRunMessagesForViewer(r.Context(), user.ID, user.IsAdmin, id, after)
	}
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("list run messages", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.MessageDTO, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageToDTO(m))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"messages": out})
}

// CreateRunInput submits a steering input (approve/reject/follow-up/cancel). A
// cancel or plan rejection against a run with no live poller is applied
// server-side; anything else is queued for the worker.
func (h *Handler) CreateRunInput(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req apitypes.RunInputRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !runInputKinds[req.Kind] {
		httpx.Error(w, http.StatusBadRequest, "kind must be one of follow_up, approve_plan, reject_plan, cancel, revise_plan, answer, stop, scope")
		return
	}

	// PRD #84 M4 4c: the "run without the capability" user override (Decision 12). When the
	// owner approves WITH override_capabilities, the override entry point BYPASSES the
	// capability approval gate and clears the run's inferred/hinted required_capabilities —
	// but ATOMICALLY with a successful approve: the clear runs only AFTER the approve's own
	// validation and enqueue succeed, so a failed approve (e.g. an invalid selection) leaves
	// the requirement INTACT and the retry stays gated. Owner- and awaiting_approval-scoped in
	// SQL, and inert for any kind other than approve_plan (the gate only runs for approve_plan).
	var res workersvc.SubmitInputResult
	var err error
	if req.OverrideCapabilities {
		res, err = h.wsvc.SubmitInputWithCapabilityOverride(r.Context(), user.ID, id, req.Kind, req.Body, req.Selection)
	} else {
		res, err = h.wsvc.SubmitInput(r.Context(), user.ID, id, req.Kind, req.Body, req.Selection)
	}
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.Is(err, workersvc.ErrRunTerminal):
			httpx.Error(w, http.StatusConflict, "run has already finished")
		case errors.Is(err, workersvc.ErrStopNotInteractive):
			// 409: a run-state conflict. Only an interactive task run's park honors a
			// graceful stop; on any other run nothing would wind it down.
			httpx.Error(w, http.StatusConflict, "run stop applies only to interactive task runs")
		case errors.Is(err, workersvc.ErrScopeNotMilestoneRun):
			// 409: a run-state conflict. A scope ceiling is meaningful only on a
			// milestone-structured issue run (PRD #634 M2).
			httpx.Error(w, http.StatusConflict, "scope applies only to milestone-structured issue runs")
		case errors.Is(err, workersvc.ErrInvalidScopeCeiling):
			// 400: the scope body did not parse as an integer ceiling. Out-of-range
			// values are clamped, not rejected — only a non-integer is a caller error.
			httpx.Error(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, workersvc.ErrReviseCapReached):
			httpx.Error(w, http.StatusConflict, "plan revision limit reached")
		case errors.Is(err, workersvc.ErrChatInputNotAllowed):
			// 409: a chat run must steer through the chat message endpoint, which
			// enforces the turn cap. The generic /inputs follow_up path would bypass it.
			httpx.Error(w, http.StatusConflict, "chat runs accept messages only through the chat endpoint")
		case errors.Is(err, workersvc.ErrRunNotAwaitingInput):
			httpx.Error(w, http.StatusConflict, "run is not waiting for an answer")
		case errors.Is(err, workersvc.ErrStaleAnswer):
			// 409, not 400: the request was well-formed and the caller did nothing
			// wrong — the question simply moved on (typically a Slack reply to
			// question N landing after the lead asked N+1).
			httpx.Error(w, http.StatusConflict, "that question has already been answered or replaced")
		case errors.Is(err, workersvc.ErrInvalidAnswer):
			httpx.Error(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, workersvc.ErrInvalidSelection):
			httpx.Error(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, workersvc.ErrCapabilityUnmet):
			// PRD #84 M4 4c: the server-side capability approval gate blocked — the run's
			// owning worker cannot satisfy an inferred/hinted required capability → 409, with
			// the unmet names and a remediation hint in the error body (same {"error": ...}
			// envelope as the sibling cases). The web/CLI (4d) can also derive the mismatch
			// from the run DTO's required_capabilities + the worker caps it already fetches;
			// this 409 is the authoritative enforcement, and its message names the fix.
			var unmet *workersvc.CapabilityUnmetError
			names := "the required capabilities"
			if errors.As(err, &unmet) {
				names = strings.Join(unmet.Unmet, ", ")
			}
			httpx.Error(w, http.StatusConflict, "the plan requires capabilities the assigned worker cannot satisfy: "+names+
				". Provision or start a worker with: "+names+"; or approve with override_capabilities to run without it")
		default:
			slog.Error("submit run input", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	// PRD #319 M3: when the just-approved selection EXPLICITLY excluded a guard role
	// (spec-keeper today), emit exactly one best-effort owner heads-up — an inbox row plus
	// an optional Slack DM. A non-guard exclusion (or any non-approve path) leaves the
	// slice empty and emits nothing. Best-effort: it never fails the approve, which is
	// already durably recorded by SubmitInput.
	if len(res.ExcludedGuardRoles) > 0 {
		h.notifyGuardRoleExcluded(r.Context(), user.ID, id, res.ExcludedGuardRoles)
	}
	resp := apitypes.RunInputResponse{ServerSide: res.ServerSide}
	// A follow_up write returns the created row (PRD #95 S2) so the web's optimistic
	// queue entry adopts the real id + timestamp. follow_up is never server-side (only
	// cancel/reject are), so res.ID/CreatedAt are the freshly-inserted row here.
	if req.Kind == "follow_up" {
		id := res.ID
		createdAt := res.CreatedAt
		resp.ID = &id
		resp.CreatedAt = &createdAt
	}
	httpx.JSON(w, http.StatusAccepted, resp)
}

// guardRoleExcludedNotificationKind is the inbox kind emitted when a run owner approves a
// plan whose agent selection EXPLICITLY excludes a guard role (PRD #319 M3). Like every
// other kind it is a plain literal — notifications.kind is a generic text column with no
// CHECK, so a new kind needs no migration. The inbox + Slack renderers key on the
// { title, body } payload convention; the web renders it generically and routes the
// run-anchored row to the run page.
const guardRoleExcludedNotificationKind = "guard_role_excluded"

// notifyGuardRoleExcluded fires the "guard role excluded" heads-up for a just-approved
// run whose selection dropped one or more guard roles (PRD #319 M3). Best-effort and
// nil-safe, mirroring notifyReviewReady: no notifier wired ⇒ no-op; a delivery error is
// logged, never returned (the approve already succeeded durably). The deep link's base is
// the operator-set public base URL (server-side, never LLM text); a lookup failure simply
// drops the link. The notification goes only to the run's OWNER (userID), never cross-user.
func (h *Handler) notifyGuardRoleExcluded(ctx context.Context, userID, runID uuid.UUID, roles []string) {
	if h.notifier == nil {
		return
	}
	base := ""
	if h.settings != nil {
		if b, err := h.settings.PublicBaseURL(ctx); err == nil {
			base = b
		}
	}
	if _, err := h.notifier.Notify(ctx, buildGuardRoleExcludedNotification(base, userID, runID, roles)); err != nil {
		slog.Error("notify guard role excluded", "error", err)
	}
}

// buildGuardRoleExcludedNotification assembles the "guard role excluded" notification
// (PRD #319 M3). It is PURE (no I/O) so its shape is unit-testable. The role names come
// from the CLOSED workersvc guard-role set (never free user text), so the body is trusted
// plain text — no untrusted plan/agent text is interpolated. The deep link is server-built
// from the operator-set base URL + the run UUID (never any LLM-supplied text); an
// empty/unset base yields no link. The notification is anchored to the run and goes only
// to its owner.
func buildGuardRoleExcludedNotification(baseURL string, userID, runID uuid.UUID, roles []string) notifysvc.Notification {
	const title = "Guard role excluded from run"
	body := guardRoleExcludedBody(roles)
	rid := runID
	return notifysvc.Notification{
		UserID: userID,
		Kind:   guardRoleExcludedNotificationKind,
		Payload: map[string]any{
			"title": title,
			"body":  body,
			"roles": roles,
		},
		RunID: &rid,
		Slack: &notifysvc.SlackRender{
			Emoji: "🛡️",
			Title: title,
			Body:  body,
			Link:  runDeepLink(baseURL, runID),
		},
	}
}

// guardRoleExcludedBody renders the notification body: it names the dropped guard role(s)
// and why the exclusion matters. The role names are drawn from the closed guard-role set,
// so this is trusted plain text. spec-keeper gets a bespoke sentence naming the specs it
// guards; any other guard role degrades to a generic sentence keyed on its name.
func guardRoleExcludedBody(roles []string) string {
	if len(roles) == 1 && roles[0] == "spec-keeper" {
		return "You approved this run with the spec-keeper guard role excluded — the role that guards specs/human.md and specs/ai.md from silent drift."
	}
	return "You approved this run with " + strings.Join(roles, ", ") + " excluded — a guard role whose exclusion leaves the specs it protects unwatched for this run."
}

// runDeepLink builds a Slack DM deep link to the run page from the operator-set public
// base URL and the run UUID. Both are server-controlled; no LLM text is interpolated. An
// empty base (unset, or the settings lookup failed) yields "" so the notification simply
// carries no link. Mirrors reviewDeepLink, but points at /runs/:id (where the web routes
// this kind) rather than the Judge workbench.
func runDeepLink(baseURL string, runID uuid.UUID) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return ""
	}
	return baseURL + "/runs/" + runID.String()
}

// ListRunInputs returns a run's follow_up steer queue (newest-first) with delivery
// status (PRD #95). RequireUser (so a CLI token works — Decision 8) and OWNER-ONLY:
// the run is resolved via GetRunByIDForUser, so a non-owner — including an admin_ro
// token on another user's run — gets 404, never an error banner. This closes a real
// read leak (follow-ups are never mirrored into run_messages). A non-owner viewer's
// queue card therefore renders empty/silent.
func (h *Handler) ListRunInputs(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	rows, err := h.wsvc.ListFollowUpInputs(r.Context(), user.ID, id)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("list run inputs", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.SteerInputDTO, 0, len(rows))
	for _, i := range rows {
		out = append(out, steerInputToDTO(i))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"inputs": out})
}
