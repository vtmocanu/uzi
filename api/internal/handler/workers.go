package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workertmpl"
)

// textPtrValue returns a pointer to s when valid, else nil — the JSON-null vs
// value convention the DTOs use for nullable text columns.
func textPtrValue(valid bool, s string) *string {
	if !valid {
		return nil
	}
	return &s
}

// timePtr returns a pointer to t when valid, else nil.
func timePtr(valid bool, t time.Time) *time.Time {
	if !valid {
		return nil
	}
	return &t
}

// intPtrValue returns a pointer to the int value of a nullable int4 column when
// valid, else nil — the JSON-null vs value convention for the worker's advertised
// max_concurrent_runs (PRD #42).
func intPtrValue(i pgtype.Int4) *int {
	if !i.Valid {
		return nil
	}
	v := int(i.Int32)
	return &v
}

// float4PtrValue / int8PtrValue apply the same JSON-null vs value convention to the
// worker's nullable stats columns (PRD #49): a NULL column becomes a JSON null so the
// UI shows nothing rather than a fabricated 0.
func float4PtrValue(f pgtype.Float4) *float64 {
	if !f.Valid {
		return nil
	}
	v := float64(f.Float32)
	return &v
}

func int8PtrValue(i pgtype.Int8) *int64 {
	if !i.Valid {
		return nil
	}
	v := i.Int64
	return &v
}

// maxWorkerNameBytes bounds a worker's human label.
const maxWorkerNameBytes = 200

// runInputKinds is the accepted steering-input set (mirrors the DB CHECK).
var runInputKinds = map[string]bool{
	"follow_up": true, "approve_plan": true, "reject_plan": true, "cancel": true,
}

// -------------------------------------------------------------------------
// DTOs
// -------------------------------------------------------------------------

type workerDTO struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// Busy is the any-kind non-terminal signal (PRD #42 Decision 10): true whenever
	// the worker holds ANY active run, chat included — so a lone active chat still
	// reads as busy even though active_runs (run-lane only) is 0.
	Busy bool `json:"busy"`
	// ActiveRuns is the worker's live count of active RUN-lane runs (chat excluded —
	// chat has its own session budget); MaxConcurrentRuns is its advertised slot cap
	// (null when unadvertised). Together they drive the "N/M runs" saturation badge
	// (PRD #42).
	ActiveRuns        int  `json:"active_runs"`
	MaxConcurrentRuns *int `json:"max_concurrent_runs"`
	// Worker template (PRD #18): the UI-declared choice and the worker's
	// self-reported value. Either may be null (no choice / older image); the UI
	// badges drift when both are set and differ.
	TemplateDeclared *string    `json:"template_declared"`
	TemplateReported *string    `json:"template_reported"`
	Version          *string    `json:"version"`
	LastHeartbeatAt  *time.Time `json:"last_heartbeat_at"`
	CreatedAt        time.Time  `json:"created_at"`
	// Latest container resource sample (PRD #49), all null until the worker reports
	// one (and re-nulled if it stops). StatsMemLimitBytes is null when the container
	// is unlimited or the sample came from the process fallback; StatsCPUPct is null
	// on the worker's first tick. StatsSource is "cgroup" or "process" (the UI labels
	// a process-source sample "worker process only"). Freshness is LastHeartbeatAt —
	// an offline worker's stats are last-known, dimmed by the client.
	StatsCPUPct        *float64 `json:"stats_cpu_pct"`
	StatsMemBytes      *int64   `json:"stats_mem_bytes"`
	StatsMemLimitBytes *int64   `json:"stats_mem_limit_bytes"`
	StatsSource        *string  `json:"stats_source"`
}

// workerDTOFromWorker builds the DTO from a bare worker row plus its active (non-chat)
// run count and its any-kind busy flag — both computed by the list queries, never
// derivable from a bare Worker row. The register/heartbeat/create paths hold neither
// (a just-registered worker has requeued its orphans and holds nothing), so they pass
// 0/false, exactly as they previously passed busy=false.
func workerDTOFromWorker(w store.Worker, activeRuns int, busy bool) workerDTO {
	return workerDTO{
		ID:                 w.ID.String(),
		Name:               w.Name,
		Status:             w.Status,
		Busy:               busy,
		ActiveRuns:         activeRuns,
		MaxConcurrentRuns:  intPtrValue(w.MaxConcurrentRuns),
		TemplateDeclared:   textPtrValue(w.TemplateDeclared.Valid, w.TemplateDeclared.String),
		TemplateReported:   textPtrValue(w.TemplateReported.Valid, w.TemplateReported.String),
		Version:            textPtrValue(w.Version.Valid, w.Version.String),
		LastHeartbeatAt:    timePtr(w.LastHeartbeatAt.Valid, w.LastHeartbeatAt.Time),
		CreatedAt:          w.CreatedAt.Time,
		StatsCPUPct:        float4PtrValue(w.StatsCpuPct),
		StatsMemBytes:      int8PtrValue(w.StatsMemBytes),
		StatsMemLimitBytes: int8PtrValue(w.StatsMemLimitBytes),
		StatsSource:        textPtrValue(w.StatsSource.Valid, w.StatsSource.String),
	}
}

func workerDTOFromRow(w store.ListWorkersByUserRow) workerDTO {
	return workerDTO{
		ID:                 w.ID.String(),
		Name:               w.Name,
		Status:             w.Status,
		Busy:               w.Busy,
		ActiveRuns:         int(w.ActiveRuns),
		MaxConcurrentRuns:  intPtrValue(w.MaxConcurrentRuns),
		TemplateDeclared:   textPtrValue(w.TemplateDeclared.Valid, w.TemplateDeclared.String),
		TemplateReported:   textPtrValue(w.TemplateReported.Valid, w.TemplateReported.String),
		Version:            textPtrValue(w.Version.Valid, w.Version.String),
		LastHeartbeatAt:    timePtr(w.LastHeartbeatAt.Valid, w.LastHeartbeatAt.Time),
		CreatedAt:          w.CreatedAt.Time,
		StatsCPUPct:        float4PtrValue(w.StatsCpuPct),
		StatsMemBytes:      int8PtrValue(w.StatsMemBytes),
		StatsMemLimitBytes: int8PtrValue(w.StatsMemLimitBytes),
		StatsSource:        textPtrValue(w.StatsSource.Valid, w.StatsSource.String),
	}
}

// runDTO is the web view of a run. session_id and last_seq are intentionally
// omitted — they are worker-internal (resume plumbing), not browser state.
type runDTO struct {
	ID string `json:"id"`
	// RepoID is null for a chat run (PRD #39): a chat has no repo. Non-null for
	// issue/ci_fix runs.
	RepoID *string `json:"repo_id"`
	// Kind is issue|ci_fix|chat. IssueIID is null for ci_fix (no issue) and chat
	// runs; the ci_fix fields below carry pipeline context, chat carries Title.
	Kind             string `json:"kind"`
	IssueIID         *int64 `json:"issue_iid"`
	IssueTitle       string `json:"issue_title"`
	IssueDescription string `json:"issue_description"`
	// Title is the chat conversation's display title (PRD #39), null for
	// issue/ci_fix runs. ResumeOfRunID points a Continue chat at the ended chat it
	// resumes (Decision 11), null otherwise.
	Title          *string `json:"title"`
	ResumeOfRunID  *string `json:"resume_of_run_id"`
	Status         string  `json:"status"`
	RequeueCount   int32   `json:"requeue_count"`
	IterationCount int32   `json:"iteration_count"`
	AutoApprove    bool    `json:"auto_approve"`
	WorkerID       *string `json:"worker_id"`
	Branch         *string `json:"branch"`
	MrIID          *int64  `json:"mr_iid"`
	// MrState is the last merge-request state the PRD #24 watcher observed for
	// mr_iid (opened|closed|merged|locked), null when never observed. Display-only
	// and best-effort (PRD #33 Decision 1): the chip treats merged/closed distinctly
	// and everything else as the plain open chip. Frozen per run — a superseded
	// run's value can be stale, so freshness is scoped to the board card in the UI.
	MrState       *string `json:"mr_state"`
	FailureReason *string `json:"failure_reason"`
	// StopKind is the server-stamped deliberate-stop signal (PRD #33): "cancelled"
	// or "plan_rejected", null for every other run. It — not the failure_reason
	// text — is what the client's isStoppedRun reads to style a stop as calm/neutral.
	StopKind *string `json:"stop_kind"`
	// Run health (PRD #47). This DTO is owner-scoped (ListRuns owner-only,
	// AdminListRuns admin-only, GetRun owner/admin), so health_reason rides
	// unconditionally here, matching failure_reason — the owner-gating that the shared
	// board applies is unnecessary. Health is the flag enum; HealthSince (when it was
	// raised) drives the run-view "stuck for Xm".
	Health       string     `json:"health"`
	HealthReason *string    `json:"health_reason"`
	HealthSince  *time.Time `json:"health_since"`
	PlanMd       *string    `json:"plan_md"`
	// ci_fix context (PRD #6), all null for an issue run: the failing ref, the
	// failing pipeline's web URL (from the frozen snapshot), and the fix verdict
	// (verified|fix_failed|not_code|null-while-unverified).
	PipelineRef    *string    `json:"pipeline_ref"`
	PipelineWebURL *string    `json:"pipeline_web_url"`
	FixVerdict     *string    `json:"fix_verdict"`
	ClaimedAt      *time.Time `json:"claimed_at"`
	StartedAt      *time.Time `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	// Per-run agent selection (PRD #37). RepoAgents is the roster the worker
	// detected in the clone's .claude/agents/: null when no worker ever reported
	// (a pre-feature run), `[]` when detection ran and found none. The plan gate
	// distinguishes those two — an inert repo card vs. a live one — so do not
	// collapse them. AgentSource/AgentExclusions stay null until a selection is
	// made, at the gate or by an autopilot run's self-resolved default.
	//
	// The names and descriptions here are REPO-SUPPLIED text. The gate panel
	// renders them as plain JSX, never through <Markdown>: an attacker-authored
	// description must not put a clickable link inside an approval dialog.
	RepoAgents      []workersvc.RepoAgent `json:"repo_agents"`
	AgentSource     *string               `json:"agent_source"`
	AgentExclusions []string              `json:"agent_exclusions"`
	// OwnAgents is the OWN-source subagent roster (name + description) the run's
	// owner would run: exactly what ListClaimAgentTemplates delivers to a claim,
	// minus the lead. It is the single source of truth the plan-gate "My agent
	// templates" picker reads, so an excludable chip always matches what the
	// approve validator accepts and the count is exact (PRD #37 M4-fix). Populated
	// only on the run-detail read (GetRun), where the store is in reach; null (a Go
	// nil slice) on the list/create/worker DTOs, which never drive the picker.
	OwnAgents []workersvc.RepoAgent `json:"own_agents"`
	// Usage is the run's rolled-up token/cost totals (PRD #40), present only when the
	// run has usage rows — null for a pre-feature run so the UI shows nothing rather
	// than a fabricated 0. Populated on the list (ListRuns) and detail (GetRun) reads;
	// nil on the create/worker DTO paths, which never render usage.
	Usage *usageDTO `json:"usage,omitempty"`
}

func runToDTO(r store.Run) runDTO {
	dto := runDTO{
		ID:               r.ID.String(),
		Kind:             r.Kind,
		IssueTitle:       r.IssueTitle,
		IssueDescription: r.IssueDescription,
		Title:            textPtrValue(r.Title.Valid, r.Title.String),
		Status:           r.Status,
		RequeueCount:     r.RequeueCount,
		IterationCount:   r.IterationCount,
		AutoApprove:      r.AutoApprove,
		Branch:           textPtrValue(r.Branch.Valid, r.Branch.String),
		MrState:          textPtrValue(r.MrState.Valid, r.MrState.String),
		FailureReason:    textPtrValue(r.FailureReason.Valid, r.FailureReason.String),
		StopKind:         textPtrValue(r.StopKind.Valid, r.StopKind.String),
		Health:           r.Health,
		HealthReason:     textPtrValue(r.HealthReason.Valid, r.HealthReason.String),
		HealthSince:      timePtr(r.HealthSince.Valid, r.HealthSince.Time),
		PlanMd:           textPtrValue(r.PlanMd.Valid, r.PlanMd.String),
		PipelineRef:      textPtrValue(r.PipelineRef.Valid, r.PipelineRef.String),
		FixVerdict:       textPtrValue(r.FixVerdict.Valid, r.FixVerdict.String),
		ClaimedAt:        timePtr(r.ClaimedAt.Valid, r.ClaimedAt.Time),
		StartedAt:        timePtr(r.StartedAt.Valid, r.StartedAt.Time),
		FinishedAt:       timePtr(r.FinishedAt.Valid, r.FinishedAt.Time),
		CreatedAt:        r.CreatedAt.Time,
		UpdatedAt:        r.UpdatedAt.Time,
	}
	if r.RepoID.Valid {
		s := uuid.UUID(r.RepoID.Bytes).String()
		dto.RepoID = &s
	}
	if r.ResumeOfRunID.Valid {
		s := uuid.UUID(r.ResumeOfRunID.Bytes).String()
		dto.ResumeOfRunID = &s
	}
	if r.IssueIid.Valid {
		v := r.IssueIid.Int64
		dto.IssueIID = &v
	}
	if r.WorkerID.Valid {
		s := uuid.UUID(r.WorkerID.Bytes).String()
		dto.WorkerID = &s
	}
	if r.MrIid.Valid {
		v := r.MrIid.Int64
		dto.MrIID = &v
	}
	// PRD #37. A decode error should be impossible (the API validates every write
	// and both columns carry a jsonb_typeof CHECK); it is logged and treated as
	// "not reported" rather than failing the read of an otherwise-fine run.
	if agents, err := workersvc.DecodeRepoAgents(r.RepoAgents); err != nil {
		slog.Error("decode run repo agents", "run_id", r.ID, "error", err)
	} else {
		dto.RepoAgents = agents
	}
	if excl, err := workersvc.DecodeExclusions(r.AgentExclusions); err != nil {
		slog.Error("decode run agent exclusions", "run_id", r.ID, "error", err)
	} else {
		dto.AgentExclusions = excl
	}
	dto.AgentSource = textPtrValue(r.AgentSource.Valid, r.AgentSource.String)
	// The failing pipeline's URL rides the frozen snapshot, not a column (the
	// pipeline cache row is transient). Best-effort decode; a ci_fix run always has
	// one, an issue run has no snapshot.
	if url := failureSnapshotWebURL(r.FailureSnapshot); url != "" {
		dto.PipelineWebURL = &url
	}
	return dto
}

// failureSnapshotWebURL pulls just the pipeline web URL out of a run's
// failure_snapshot jsonb for the run-view header link. Returns "" for an issue run
// (no snapshot) or a malformed one.
func failureSnapshotWebURL(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var snap struct {
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return ""
	}
	return snap.WebURL
}

type messageDTO struct {
	Seq       int32           `json:"seq"`
	Kind      string          `json:"kind"`
	Agent     *string         `json:"agent"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

func messageToDTO(m store.RunMessage) messageDTO {
	return messageDTO{
		Seq:       m.Seq,
		Kind:      m.Kind,
		Agent:     textPtrValue(m.Agent.Valid, m.Agent.String),
		Payload:   json.RawMessage(m.Payload),
		CreatedAt: m.CreatedAt.Time,
	}
}

// -------------------------------------------------------------------------
// Worker management (session-authenticated)
// -------------------------------------------------------------------------

// CreateWorker issues a worker and returns its join token exactly once. The
// token is shown here and never again (only its hash is stored), so the client
// must surface it to the user immediately.
func (h *Handler) CreateWorker(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Name string `json:"name"`
		// Template is the worker template the user picks at issuance (PRD #18).
		// Optional: empty ⇒ no declared choice (stored NULL). When present it must
		// be a known curated template — validated against the registry so an
		// arbitrary string can't land in the declared column.
		Template string `json:"template"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > maxWorkerNameBytes {
		httpx.Error(w, http.StatusBadRequest, "name must be non-empty and at most 200 characters")
		return
	}
	template := strings.TrimSpace(req.Template)
	if template != "" && !workertmpl.Valid(template) {
		httpx.Error(w, http.StatusBadRequest, "unknown worker template")
		return
	}

	wkr, token, err := h.wsvc.CreateWorker(r.Context(), user.ID, name, template)
	if err != nil {
		slog.Error("create worker", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"worker": workerDTOFromWorker(wkr, 0, false),
		"token":  token,
	})
}

// ListWorkers returns the current user's workers with derived busy status.
func (h *Handler) ListWorkers(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.wsvc.ListWorkers(r.Context(), user.ID)
	if err != nil {
		slog.Error("list workers", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]workerDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, workerDTOFromRow(row))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"workers": out})
}

// DeleteWorker revokes a worker owned by the current user.
func (h *Handler) DeleteWorker(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid worker id")
		return
	}
	if err := h.wsvc.DeleteWorker(r.Context(), user.ID, id); err != nil {
		switch {
		case errors.Is(err, workersvc.ErrWorkerNotFound):
			httpx.Error(w, http.StatusNotFound, "worker not found")
		case errors.Is(err, workersvc.ErrWorkerHasActiveRuns):
			httpx.Error(w, http.StatusConflict, "worker has active runs; cancel them before deleting it")
		default:
			slog.Error("delete worker", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.IssueIID <= 0 {
		httpx.Error(w, http.StatusBadRequest, "issue_iid must be a positive integer")
		return
	}

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	issue, err := f.GetIssue(r.Context(), repo.ForgeProjectID, req.IssueIID)
	if err != nil {
		// err is already PAT-redacted by the driver.
		httpx.Error(w, http.StatusBadGateway, "could not read the issue from the forge: "+err.Error())
		return
	}

	// PRDLESS bypass (PRD #22 Decision 3): compute allowWithoutPRD from the fresh
	// forge snapshot's labels and the prdless settings, then thread it into the
	// shared createRun gate. Reading the fresh labels (not the cache) means a
	// just-added label works immediately, without waiting for a poller cycle;
	// matching is exact, like board column labels. Settings reads are best-effort
	// (the accessors return the default alongside a cold error): a settings blip
	// falls back to the default label / enabled=true, and an unlabeled issue is
	// still gated.
	prdlessEnabled, _ := h.settings.PrdlessEnabled(r.Context())
	prdlessLabel, _ := h.settings.PrdlessLabel(r.Context())
	allowWithoutPRD := prdlessEnabled && slices.Contains(issue.Labels, prdlessLabel)

	// The description cap is enforced inside CreateRun (shared with the autopilot
	// path), surfaced here as ErrDescriptionTooLarge → 422.
	run, err := h.wsvc.CreateRun(r.Context(), user.ID, repo.ID, req.IssueIID, issue.Description, allowWithoutPRD)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRepoNotFound):
			httpx.Error(w, http.StatusNotFound, "repo not found")
		case errors.Is(err, workersvc.ErrIssueNotFound):
			httpx.Error(w, http.StatusNotFound, "issue not found on this repo's board")
		case errors.Is(err, workersvc.ErrDescriptionTooLarge):
			httpx.Error(w, http.StatusUnprocessableEntity, "issue description is too large to run")
		case errors.Is(err, workersvc.ErrNoPRDLink):
			// Extend the hint with the escape-hatch label only when the feature is
			// enabled instance-wide, so a strict-regime instance never advertises it.
			msg := "issue has no PRD link; add a prds/*.md link before starting a run"
			if prdlessEnabled {
				msg = fmt.Sprintf("issue has no PRD link; add a prds/*.md link (or the %s label) before starting a run", prdlessLabel)
			}
			httpx.Error(w, http.StatusUnprocessableEntity, msg)
		case errors.Is(err, workersvc.ErrActiveRunExists):
			httpx.Error(w, http.StatusConflict, "a run is already in progress for this issue")
		case errors.Is(err, workersvc.ErrBranchInUse):
			// Cross-kind exclusion (PRD #6): a ci_fix run is already holding this
			// issue's agent branch/worktree.
			httpx.Error(w, http.StatusConflict, "a CI-fix run is already working this issue's branch; cancel it before starting an issue run")
		default:
			slog.Error("create run", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"run": runToDTO(run)})
}

// GetRun returns one run visible to the viewer (owner, or any run for an admin).
func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
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
	dto := runToDTO(run)
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
		dto.Usage = &usageDTO{
			InputTokens:         u.InputTokens,
			CacheReadTokens:     u.CacheReadTokens,
			CacheCreationTokens: u.CacheCreationTokens,
			OutputTokens:        u.OutputTokens,
			CostUSD:             numericToFloat(u.CostUsd),
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("get run usage total", "run_id", run.ID, "error", err)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": dto})
}

// ListRunMessages returns a run's persisted messages after ?after=<seq> (default
// 0), the replay source a reconnecting browser reads before going live. Visible
// to the run's owner or an admin.
func (h *Handler) ListRunMessages(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
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
	msgs, err := h.wsvc.ListRunMessagesForViewer(r.Context(), user.ID, user.IsAdmin, id, after)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("list run messages", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]messageDTO, 0, len(msgs))
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
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	var req struct {
		Kind string `json:"kind"`
		Body string `json:"body"`
		// Selection is the PRD #37 agent choice, sent STRUCTURED and legal only with
		// approve_plan. The client never composes the worker-bound input body itself:
		// the server validates this against the run's real roster and writes its own
		// canonical JSON encoding into that body.
		Selection *workersvc.AgentSelection `json:"selection"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !runInputKinds[req.Kind] {
		httpx.Error(w, http.StatusBadRequest, "kind must be one of follow_up, approve_plan, reject_plan, cancel")
		return
	}

	res, err := h.wsvc.SubmitInput(r.Context(), user.ID, id, req.Kind, req.Body, req.Selection)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.Is(err, workersvc.ErrRunTerminal):
			httpx.Error(w, http.StatusConflict, "run has already finished")
		case errors.Is(err, workersvc.ErrInvalidSelection):
			httpx.Error(w, http.StatusBadRequest, err.Error())
		default:
			slog.Error("submit run input", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]any{"server_side": res.ServerSide})
}
