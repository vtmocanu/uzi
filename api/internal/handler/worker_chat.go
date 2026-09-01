package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The worker's chat-agent read surface (PRD #39 M3, Decision 7): the chat agent
// investigates its OWNER'S runs through the worker. Every endpoint is scoped to the
// authenticated worker's user_id — a foreign run id is 404, never a bare lookup.

// workerRunListItemDTO is one run in the compact worker list. title is issue_title
// (which a chat run also carries, its derived conversation title). repo_path/mr_url
// are null for a chat run (no repo/MR).
type workerRunListItemDTO struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	Status        string    `json:"status"`
	RepoPath      *string   `json:"repo_path"`
	IssueIID      *int64    `json:"issue_iid"`
	Title         string    `json:"title"`
	Branch        *string   `json:"branch"`
	MrURL         *string   `json:"mr_url"`
	FailureReason *string   `json:"failure_reason"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// workerRunDetailDTO is the single-run view: the list fields plus the diagnostic
// fields the agent needs to answer "why did run X fail?".
type workerRunDetailDTO struct {
	workerRunListItemDTO
	MrState        *string `json:"mr_state"`
	StopKind       *string `json:"stop_kind"`
	FixVerdict     *string `json:"fix_verdict"`
	IterationCount int32   `json:"iteration_count"`
	PlanMd         *string `json:"plan_md"`
}

// mrURL builds the GitLab merge-request URL from the repo web URL + iid, or nil when
// either is absent (a chat run, or a run with no MR yet).
func mrURL(webURL pgtype.Text, mrIID pgtype.Int8) *string {
	if !webURL.Valid || !mrIID.Valid {
		return nil
	}
	u := webURL.String + "/-/merge_requests/" + strconv.FormatInt(mrIID.Int64, 10)
	return &u
}

func int64Ptr(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

// WorkerChatListRuns lists the worker's user's runs (both kinds), newest first,
// bounded by ?limit (default/cap 50).
func (h *Handler) WorkerChatListRuns(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			httpx.Error(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	rows, err := h.wsvc.ListRunsForWorker(r.Context(), wkr, limit)
	if err != nil {
		slog.Error("worker chat list runs", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]workerRunListItemDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, workerRunListItemDTO{
			ID:            row.ID.String(),
			Kind:          row.Kind,
			Status:        row.Status,
			RepoPath:      textPtrValue(row.RepoPath.Valid, row.RepoPath.String),
			IssueIID:      int64Ptr(row.IssueIid),
			Title:         row.IssueTitle,
			Branch:        textPtrValue(row.Branch.Valid, row.Branch.String),
			MrURL:         mrURL(row.RepoWebUrl, row.MrIid),
			FailureReason: textPtrValue(row.FailureReason.Valid, row.FailureReason.String),
			CreatedAt:     row.CreatedAt.Time,
			UpdatedAt:     row.UpdatedAt.Time,
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"runs": out})
}

// WorkerChatGetRun returns one run's detail, scoped to the worker's user.
func (h *Handler) WorkerChatGetRun(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	row, err := h.wsvc.GetRunForWorker(r.Context(), wkr, runID)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("worker chat get run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": workerRunDetailDTO{
		workerRunListItemDTO: workerRunListItemDTO{
			ID:            row.ID.String(),
			Kind:          row.Kind,
			Status:        row.Status,
			RepoPath:      textPtrValue(row.RepoPath.Valid, row.RepoPath.String),
			IssueIID:      int64Ptr(row.IssueIid),
			Title:         row.IssueTitle,
			Branch:        textPtrValue(row.Branch.Valid, row.Branch.String),
			MrURL:         mrURL(row.RepoWebUrl, row.MrIid),
			FailureReason: textPtrValue(row.FailureReason.Valid, row.FailureReason.String),
			CreatedAt:     row.CreatedAt.Time,
			UpdatedAt:     row.UpdatedAt.Time,
		},
		MrState:        textPtrValue(row.MrState.Valid, row.MrState.String),
		StopKind:       textPtrValue(row.StopKind.Valid, row.StopKind.String),
		FixVerdict:     textPtrValue(row.FixVerdict.Valid, row.FixVerdict.String),
		IterationCount: row.IterationCount,
		PlanMd:         textPtrValue(row.PlanMd.Valid, row.PlanMd.String),
	}})
}

// WorkerChatRunMessages returns a bounded page of a run's messages after ?after=<seq>
// (default 0), bounded by ?limit (default/cap 200). Scoped to the worker's user.
func (h *Handler) WorkerChatRunMessages(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	after := int32(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n < 0 {
			httpx.Error(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = int32(n)
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			httpx.Error(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	msgs, err := h.wsvc.ListRunMessagesForWorker(r.Context(), wkr, runID, after, limit)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("worker chat run messages", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.MessageDTO, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageToDTO(m))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"messages": out})
}

// proposalDTO is the created proposal returned to the worker's propose_issue tool.
// It intentionally omits the internal repo_id UUID: the worker only handles the
// human-readable repo_path (Decision 7), and the browser reads the proposal solely
// from the emitted `proposal` run_message (which mirrors this shape), so no consumer
// needs the UUID here.
type proposalDTO struct {
	ID          string    `json:"id"`
	RunID       string    `json:"run_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Labels      []string  `json:"labels"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

func proposalToDTO(p store.IssueProposal) proposalDTO {
	labels := []string{}
	if len(p.Labels) > 0 {
		_ = json.Unmarshal(p.Labels, &labels)
	}
	return proposalDTO{
		ID:          p.ID.String(),
		RunID:       p.RunID.String(),
		Title:       p.Title,
		Description: p.Description,
		Labels:      labels,
		Status:      p.Status,
		CreatedAt:   p.CreatedAt.Time,
	}
}

type workerProposalRequest struct {
	// RepoPath (path_with_namespace, e.g. "group/project") is what the agent sends —
	// the read endpoints expose repo_path, not internal UUIDs (Decision 7). RepoID is
	// accepted too for back-compat; RepoPath wins when both are present.
	RepoPath    string   `json:"repo_path"`
	RepoID      string   `json:"repo_id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Labels      []string `json:"labels"`
}

// WorkerCreateProposal is the worker's propose_issue tool (M3, Decision 8): it
// persists a PENDING issue proposal on the target chat run. It NEVER writes to the
// forge — only the browser's confirm does. The run must be the worker's user's chat
// run and non-terminal, repo_id must be a repo that user owns, and the run must be
// under the per-run pending cap. Bounded by the per-worker proposal rate limiter on
// the route.
func (h *Handler) WorkerCreateProposal(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req workerProposalRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Resolve the target repo. The agent sends repo_path (it never sees internal
	// UUIDs); repo_id is accepted for back-compat. Path resolution is user-scoped, so
	// an unknown/foreign path is 404.
	var repoID uuid.UUID
	var err error
	switch {
	case strings.TrimSpace(req.RepoPath) != "":
		repoID, err = h.wsvc.ResolveRepoForWorker(r.Context(), wkr, strings.TrimSpace(req.RepoPath))
		if err != nil {
			if errors.Is(err, workersvc.ErrRepoNotFound) {
				httpx.Error(w, http.StatusNotFound, "repo not found for this worker's user")
				return
			}
			slog.Error("worker create proposal: resolve repo path", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	case req.RepoID != "":
		repoID, err = uuid.Parse(req.RepoID)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid repo_id")
			return
		}
	default:
		httpx.Error(w, http.StatusBadRequest, "repo_path is required")
		return
	}
	title := req.Title
	if title == "" || len(title) > workersvc.MaxProposalTitleBytes {
		httpx.Error(w, http.StatusBadRequest, "title must be non-empty and at most 255 characters")
		return
	}
	if len(req.Description) > workersvc.MaxIssueDescriptionBytes {
		httpx.Error(w, http.StatusBadRequest, "description is too large")
		return
	}
	if len(req.Labels) > workersvc.MaxProposalLabels {
		httpx.Error(w, http.StatusBadRequest, "too many labels")
		return
	}
	for _, l := range req.Labels {
		if l == "" || len(l) > workersvc.MaxProposalLabelBytes {
			httpx.Error(w, http.StatusBadRequest, "each label must be non-empty and at most 255 characters")
			return
		}
	}
	labels := req.Labels
	if labels == nil {
		labels = []string{}
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		slog.Error("marshal proposal labels", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	prop, err := h.wsvc.CreateProposal(r.Context(), wkr, runID, repoID, title, req.Description, labelsJSON)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker's user")
		case errors.Is(err, workersvc.ErrProposalRunNotChat):
			httpx.Error(w, http.StatusConflict, "proposals can only target a chat run")
		case errors.Is(err, workersvc.ErrRunTerminal):
			httpx.Error(w, http.StatusConflict, "the chat has ended; cannot add a proposal")
		case errors.Is(err, workersvc.ErrRepoNotFound):
			httpx.Error(w, http.StatusNotFound, "repo not found for this worker's user")
		case errors.Is(err, workersvc.ErrProposalCapReached):
			httpx.Error(w, http.StatusConflict, "too many pending proposals for this chat; resolve some first")
		default:
			slog.Error("worker create proposal", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"proposal": proposalToDTO(prop)})
}
