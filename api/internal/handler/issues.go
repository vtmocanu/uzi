package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/board"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// maxIssueTitleBytes bounds a created issue's title (GitLab caps titles at 255).
const maxIssueTitleBytes = 255

type createIssueRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// CreateIssue opens a new PRD-labelled issue on the forge for the given repo and
// caches it so the board reflects it immediately. The forge is the source of
// truth (the next sync reconciles it); this never fabricates a local-only card —
// the issue returned here is the real one the forge just created. The user must
// own the repo (enforced by repoForRequest).
func (h *Handler) CreateIssue(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	var req createIssueRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" || len(title) > maxIssueTitleBytes {
		httpx.Error(w, http.StatusBadRequest, "title must be non-empty and at most 255 characters")
		return
	}
	if len(req.Description) > workersvc.MaxIssueDescriptionBytes {
		httpx.Error(w, http.StatusBadRequest, "description is too large")
		return
	}

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// The created issue carries the configured PRD label (PRD #19): best-effort —
	// a cold settings read yields the compiled-in default, never an empty label.
	prdLabel, _ := h.settings.PRDLabel(r.Context())
	created, err := f.CreateIssue(r.Context(), repo.ForgeProjectID, title, req.Description, []string{prdLabel})
	if err != nil {
		// err is already PAT-redacted by the driver.
		httpx.Error(w, http.StatusBadGateway, "could not create the issue on the forge: "+err.Error())
		return
	}

	// Cache the real, just-created issue (same write the sync path performs) so
	// the card appears without waiting for the next poll. has_prd_link is computed
	// from the description exactly as sync does.
	labelsJSON, err := json.Marshal(created.Labels)
	if err != nil {
		slog.Error("marshal issue labels", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	updated := created.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}
	upserted, err := h.q.UpsertIssue(r.Context(), store.UpsertIssueParams{
		RepoID:         repo.ID,
		ForgeIssueIid:  created.IID,
		Title:          created.Title,
		State:          created.State,
		Labels:         labelsJSON,
		WebUrl:         created.WebURL,
		Author:         pgtypeTextOrNull(created.Author),
		HasPrdLink:     forgesvc.HasPRDLink(req.Description),
		ForgeUpdatedAt: pgtype.Timestamptz{Time: updated, Valid: true},
	})
	if err != nil {
		// The issue exists on the forge; only the local cache write failed. The
		// next sync will pick it up, so report success with the forge facts.
		slog.Warn("cache new issue after create", "repo", repo.PathWithNamespace, "error", err)
		httpx.JSON(w, http.StatusCreated, map[string]any{
			"card": cardDTO{IID: created.IID, Title: created.Title, State: created.State, Labels: created.Labels, WebURL: created.WebURL, ForgeType: repo.ForgeType, HasPRDLink: forgesvc.HasPRDLink(req.Description)},
		})
		return
	}

	cols, err := h.q.ListBoardColumns(r.Context(), repo.ID)
	if err != nil {
		slog.Error("list board columns", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	position := make(map[string]int, len(cols))
	for _, c := range cols {
		position[c.LabelName] = int(c.Position)
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"card": issueToCard(upserted, position, repo.ForgeType)})
}

// issueDetailDTO is the in-app issue view payload (PRD #12 §3): the board card
// fields plus the issue Description. The description is fetched live from the
// forge and never cached (it carries the PRD link and is the basis for deciding
// to run); the SPA renders it as markdown.
type issueDetailDTO struct {
	IID         int64    `json:"iid"`
	Title       string   `json:"title"`
	State       string   `json:"state"`
	Labels      []string `json:"labels"`
	WebURL      string   `json:"web_url"`
	Author      *string  `json:"author"`
	HasPRDLink  bool     `json:"has_prd_link"`
	Column      string   `json:"column"`
	Closed      bool     `json:"closed"`
	Conflict    bool     `json:"conflict"`
	Description string   `json:"description"`
}

// buildIssueDetail assembles the issue-view payload from a freshly-fetched forge
// issue and the repo's column position map, resolving the card's column and
// computing has_prd_link the same way the board and sync paths do. Pure (no DB or
// forge I/O) so the resolution is unit-tested directly — the handler itself can't
// be, since h.q is a concrete *store.Queries.
func buildIssueDetail(issue forge.Issue, position map[string]int) issueDetailDTO {
	col, closed, conflict := board.ResolveColumn(issue.Labels, issue.State, position)
	labels := issue.Labels
	if labels == nil {
		labels = []string{}
	}
	dto := issueDetailDTO{
		IID:         issue.IID,
		Title:       issue.Title,
		State:       issue.State,
		Labels:      labels,
		WebURL:      issue.WebURL,
		HasPRDLink:  forgesvc.HasPRDLink(issue.Description),
		Column:      col,
		Closed:      closed,
		Conflict:    conflict,
		Description: issue.Description,
	}
	if issue.Author != "" {
		a := issue.Author
		dto.Author = &a
	}
	return dto
}

// GetIssueDetail returns one issue's card fields plus its Description, read live
// from the forge via the connection PAT (the description is never cached). Powers
// the in-app issue view (PRD #12 §3); the run history is a separate ListRuns call
// with the ?repo_id=&issue_iid= filters. Owner-scoped like the rest of the board
// endpoints (repoForRequest authorizes the repo to the current user).
func (h *Handler) GetIssueDetail(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	iid, err := parseInt64(chi.URLParam(r, "iid"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid issue id")
		return
	}
	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	issue, err := f.GetIssue(r.Context(), repo.ForgeProjectID, iid)
	if err != nil {
		// err is already PAT-redacted by the driver.
		httpx.Error(w, http.StatusBadGateway, "could not read the issue from the forge: "+err.Error())
		return
	}
	cols, err := h.q.ListBoardColumns(r.Context(), repo.ID)
	if err != nil {
		slog.Error("list board columns", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	position := make(map[string]int, len(cols))
	for _, c := range cols {
		position[c.LabelName] = int(c.Position)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"issue": buildIssueDetail(issue, position)})
}
