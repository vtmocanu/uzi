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

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
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
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Status          string     `json:"status"`
	Busy            bool       `json:"busy"`
	Version         *string    `json:"version"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

func workerDTOFromWorker(w store.Worker, busy bool) workerDTO {
	return workerDTO{
		ID:              w.ID.String(),
		Name:            w.Name,
		Status:          w.Status,
		Busy:            busy,
		Version:         textPtrValue(w.Version.Valid, w.Version.String),
		LastHeartbeatAt: timePtr(w.LastHeartbeatAt.Valid, w.LastHeartbeatAt.Time),
		CreatedAt:       w.CreatedAt.Time,
	}
}

func workerDTOFromRow(w store.ListWorkersByUserRow) workerDTO {
	return workerDTO{
		ID:              w.ID.String(),
		Name:            w.Name,
		Status:          w.Status,
		Busy:            w.Busy,
		Version:         textPtrValue(w.Version.Valid, w.Version.String),
		LastHeartbeatAt: timePtr(w.LastHeartbeatAt.Valid, w.LastHeartbeatAt.Time),
		CreatedAt:       w.CreatedAt.Time,
	}
}

// runDTO is the web view of a run. session_id and last_seq are intentionally
// omitted — they are worker-internal (resume plumbing), not browser state.
type runDTO struct {
	ID   string `json:"id"`
	RepoID string `json:"repo_id"`
	// Kind is issue|ci_fix (PRD #6). IssueIID is null for a ci_fix run (no issue);
	// the ci_fix fields below carry its pipeline context instead.
	Kind             string     `json:"kind"`
	IssueIID         *int64     `json:"issue_iid"`
	IssueTitle       string     `json:"issue_title"`
	IssueDescription string     `json:"issue_description"`
	Status           string     `json:"status"`
	RequeueCount     int32      `json:"requeue_count"`
	IterationCount   int32      `json:"iteration_count"`
	AutoApprove      bool       `json:"auto_approve"`
	WorkerID         *string    `json:"worker_id"`
	Branch           *string    `json:"branch"`
	MrIID            *int64     `json:"mr_iid"`
	// MrState is the last merge-request state the PRD #24 watcher observed for
	// mr_iid (opened|closed|merged|locked), null when never observed. Display-only
	// and best-effort (PRD #33 Decision 1): the chip treats merged/closed distinctly
	// and everything else as the plain open chip. Frozen per run — a superseded
	// run's value can be stale, so freshness is scoped to the board card in the UI.
	MrState          *string    `json:"mr_state"`
	FailureReason    *string    `json:"failure_reason"`
	// StopKind is the server-stamped deliberate-stop signal (PRD #33): "cancelled"
	// or "plan_rejected", null for every other run. It — not the failure_reason
	// text — is what the client's isStoppedRun reads to style a stop as calm/neutral.
	StopKind         *string    `json:"stop_kind"`
	PlanMd           *string    `json:"plan_md"`
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
}

func runToDTO(r store.Run) runDTO {
	dto := runDTO{
		ID:               r.ID.String(),
		RepoID:           r.RepoID.String(),
		Kind:             r.Kind,
		IssueTitle:       r.IssueTitle,
		IssueDescription: r.IssueDescription,
		Status:           r.Status,
		RequeueCount:     r.RequeueCount,
		IterationCount:   r.IterationCount,
		AutoApprove:      r.AutoApprove,
		Branch:           textPtrValue(r.Branch.Valid, r.Branch.String),
		MrState:          textPtrValue(r.MrState.Valid, r.MrState.String),
		FailureReason:    textPtrValue(r.FailureReason.Valid, r.FailureReason.String),
		StopKind:         textPtrValue(r.StopKind.Valid, r.StopKind.String),
		PlanMd:           textPtrValue(r.PlanMd.Valid, r.PlanMd.String),
		PipelineRef:      textPtrValue(r.PipelineRef.Valid, r.PipelineRef.String),
		FixVerdict:       textPtrValue(r.FixVerdict.Valid, r.FixVerdict.String),
		ClaimedAt:        timePtr(r.ClaimedAt.Valid, r.ClaimedAt.Time),
		StartedAt:        timePtr(r.StartedAt.Valid, r.StartedAt.Time),
		FinishedAt:       timePtr(r.FinishedAt.Valid, r.FinishedAt.Time),
		CreatedAt:        r.CreatedAt.Time,
		UpdatedAt:        r.UpdatedAt.Time,
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

	wkr, token, err := h.wsvc.CreateWorker(r.Context(), user.ID, name)
	if err != nil {
		slog.Error("create worker", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"worker": workerDTOFromWorker(wkr, false),
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
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run)})
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
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !runInputKinds[req.Kind] {
		httpx.Error(w, http.StatusBadRequest, "kind must be one of follow_up, approve_plan, reject_plan, cancel")
		return
	}

	res, err := h.wsvc.SubmitInput(r.Context(), user.ID, id, req.Kind, req.Body)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.Is(err, workersvc.ErrRunTerminal):
			httpx.Error(w, http.StatusConflict, "run has already finished")
		default:
			slog.Error("submit run input", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]any{"server_side": res.ServerSide})
}
