package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
	if len(req.Description) > maxIssueDescriptionBytes {
		httpx.Error(w, http.StatusBadRequest, "description is too large")
		return
	}

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	created, err := f.CreateIssue(r.Context(), repo.ForgeProjectID, title, req.Description, []string{forgesvc.PRDLabel})
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
			"card": cardDTO{IID: created.IID, Title: created.Title, State: created.State, Labels: created.Labels, WebURL: created.WebURL, HasPRDLink: forgesvc.HasPRDLink(req.Description)},
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
	httpx.JSON(w, http.StatusCreated, map[string]any{"card": issueToCard(upserted, position)})
}
