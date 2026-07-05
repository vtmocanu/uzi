package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/privcheck"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// ── DTOs ────────────────────────────────────────────────────────────────────

type connectionDTO struct {
	ID             string     `json:"id"`
	ForgeType      string     `json:"forge_type"`
	BaseURL        string     `json:"base_url"`
	BotUsername    string     `json:"bot_username"`
	BotForgeUserID int64      `json:"bot_forge_user_id"`
	CreatedAt      time.Time  `json:"created_at"`
	LastVerifiedAt *time.Time `json:"last_verified_at"`
	// Privilege surfacing (PRD #5). A null status means never checked (the boot
	// sweep back-fills it); the report carries the token + per-repo findings the
	// UI expands and badges. checked_at is when the report was stamped.
	PrivilegeStatus    *string           `json:"privilege_status"`
	PrivilegeCheckedAt *time.Time        `json:"privilege_checked_at"`
	PrivilegeReport    *privcheck.Report `json:"privilege_report"`
}

func connToDTO(c store.ForgeConnection) connectionDTO {
	dto := connectionDTO{
		ID:             c.ID.String(),
		ForgeType:      c.ForgeType,
		BaseURL:        c.BaseUrl,
		BotUsername:    c.BotUsername,
		BotForgeUserID: c.BotForgeUserID,
		CreatedAt:      c.CreatedAt.Time,
	}
	if c.LastVerifiedAt.Valid {
		t := c.LastVerifiedAt.Time
		dto.LastVerifiedAt = &t
	}
	if c.PrivilegeStatus.Valid {
		s := c.PrivilegeStatus.String
		dto.PrivilegeStatus = &s
	}
	if c.PrivilegeCheckedAt.Valid {
		t := c.PrivilegeCheckedAt.Time
		dto.PrivilegeCheckedAt = &t
	}
	if len(c.PrivilegeReport) > 0 {
		var rep privcheck.Report
		if err := json.Unmarshal(c.PrivilegeReport, &rep); err == nil {
			dto.PrivilegeReport = &rep
		}
	}
	return dto
}

type repoDTO struct {
	ID                string  `json:"id"`
	ConnectionID      string  `json:"connection_id"`
	ForgeProjectID    int64   `json:"forge_project_id"`
	PathWithNamespace string  `json:"path_with_namespace"`
	WebURL            string  `json:"web_url"`
	DefaultBranch     *string `json:"default_branch"`
	Enabled           bool    `json:"enabled"`
}

func repoToDTO(r store.Repo) repoDTO {
	dto := repoDTO{
		ID:                r.ID.String(),
		ConnectionID:      r.ConnectionID.String(),
		ForgeProjectID:    r.ForgeProjectID,
		PathWithNamespace: r.PathWithNamespace,
		WebURL:            r.WebUrl,
		Enabled:           r.Enabled,
	}
	if r.DefaultBranch.Valid {
		dto.DefaultBranch = &r.DefaultBranch.String
	}
	return dto
}

// ── Config ──────────────────────────────────────────────────────────────────

// ForgeConfig exposes the values the connect UI needs to offer only valid
// choices: the SSRF allowlist of base URLs and the supported forge types. It is
// read-only and reveals nothing secret (the allowlist is operator-set config).
func (h *Handler) ForgeConfig(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{
		"allowed_base_urls": h.cfg.ForgeAllowedBaseURLs,
		"forge_types":       []string{string(forge.TypeGitLab)},
	})
}

// ── Connections ─────────────────────────────────────────────────────────────

type createConnectionRequest struct {
	ForgeType string `json:"forge_type"`
	BaseURL   string `json:"base_url"`
	Token     string `json:"token"`
}

// CreateConnection connects (or reconnects/rotates) a bot PAT. It validates the
// base URL against the SSRF allowlist, verifies the token against the forge to
// capture the bot identity, then stores the PAT encrypted at rest. The token is
// never echoed back.
func (h *Handler) CreateConnection(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req createConnectionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	forgeType := strings.TrimSpace(req.ForgeType)
	if forgeType == "" {
		forgeType = string(forge.TypeGitLab)
	}
	if forgeType != string(forge.TypeGitLab) {
		httpx.Error(w, http.StatusBadRequest, "unsupported forge type")
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		httpx.Error(w, http.StatusBadRequest, "a bot token is required")
		return
	}
	if !h.cfg.ForgeBaseURLAllowed(req.BaseURL) {
		httpx.Error(w, http.StatusBadRequest, "base URL is not in the allowed forge list")
		return
	}
	baseURL, err := config.NormalizeForgeBaseURL(req.BaseURL)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid base URL")
		return
	}

	f, err := h.svc.ForgeForToken(forge.Type(forgeType), baseURL, req.Token)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "could not initialize forge client")
		return
	}
	identity, err := f.VerifyToken(r.Context())
	if err != nil {
		// err is already PAT-redacted by the driver.
		httpx.Error(w, http.StatusBadGateway, "token verification failed: "+err.Error())
		return
	}

	// Least-privilege gate (PRD #5): token-level violations block the save — this
	// is the one moment uzi holds the plaintext and the user is present to fix it.
	// Per-repo checks can't run here (no repos are enabled yet); those warn later.
	if tr := h.pcheck.CheckToken(r.Context(), f, identity.IsAdmin); len(tr.Violations) > 0 {
		httpx.JSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":      "the bot token is over-privileged and was not saved; mint a least-privilege token (see the bot setup doc)",
			"violations": tr.Violations,
		})
		return
	}

	ciphertext, err := h.svc.EncryptToken(req.Token)
	if err != nil {
		slog.Error("encrypt token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	conn, err := h.q.UpsertForgeConnection(r.Context(), store.UpsertForgeConnectionParams{
		UserID:          user.ID,
		ForgeType:       forgeType,
		BaseUrl:         baseURL,
		BotUsername:     identity.Username,
		BotForgeUserID:  identity.ForgeUserID,
		TokenCiphertext: ciphertext,
	})
	if err != nil {
		slog.Error("upsert forge connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"connection": connToDTO(conn)})
}

// ListConnections returns the current user's forge connections.
func (h *Handler) ListConnections(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	conns, err := h.q.ListForgeConnectionsByUser(r.Context(), user.ID)
	if err != nil {
		slog.Error("list connections", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]connectionDTO, 0, len(conns))
	for _, c := range conns {
		out = append(out, connToDTO(c))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"connections": out})
}

// VerifyConnection re-checks a stored connection's token against the forge and
// stamps last_verified_at on success.
func (h *Handler) VerifyConnection(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid connection id")
		return
	}
	conn, err := h.q.GetForgeConnectionForUser(r.Context(), store.GetForgeConnectionForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "connection not found")
		return
	}
	f, err := h.svc.ForgeForConnection(conn.ForgeType, conn.BaseUrl, conn.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := f.VerifyToken(r.Context()); err != nil {
		httpx.Error(w, http.StatusBadGateway, "token verification failed: "+err.Error())
		return
	}
	updated, err := h.q.TouchForgeConnectionVerified(r.Context(), store.TouchForgeConnectionVerifiedParams{ID: id, UserID: user.ID})
	if err != nil {
		slog.Error("touch connection verified", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"connection": connToDTO(updated)})
}

// PrivilegeCheck runs the full PAT least-privilege report for a connection
// (token + every enabled repo), persists it, and returns it. Owner-only, behind
// the per-user forge rate limiter (it is the heaviest forge-proxying route: one
// token call plus two per enabled repo). It never blocks — the report surfaces
// findings, the badge reflects them, and the user acts.
func (h *Handler) PrivilegeCheck(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid connection id")
		return
	}
	conn, err := h.q.GetForgeConnectionForUser(r.Context(), store.GetForgeConnectionForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "connection not found")
		return
	}
	report, err := h.pcheck.CheckConnection(r.Context(), conn)
	if err != nil {
		slog.Error("privilege check", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"report": report})
}

// DeleteConnection removes a connection (cascading its repos, board columns, and
// cached issues via FK).
func (h *Handler) DeleteConnection(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid connection id")
		return
	}
	rows, err := h.q.DeleteForgeConnectionForUser(r.Context(), store.DeleteForgeConnectionForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		slog.Error("delete connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if rows == 0 {
		httpx.Error(w, http.StatusNotFound, "connection not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListProjects fetches the bot's live membership list from the forge and upserts
// each project as a repo row (enabled=false) so it is addressable, then returns
// the repos with their current enabled state.
func (h *Handler) ListProjects(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid connection id")
		return
	}
	conn, err := h.q.GetForgeConnectionForUser(r.Context(), store.GetForgeConnectionForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "connection not found")
		return
	}
	f, err := h.svc.ForgeForConnection(conn.ForgeType, conn.BaseUrl, conn.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	projects, err := f.ListProjects(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, "could not list projects: "+err.Error())
		return
	}
	for _, p := range projects {
		branch := pgtypeTextOrNull(p.DefaultBranch)
		if _, err := h.q.UpsertRepo(r.Context(), store.UpsertRepoParams{
			ConnectionID:      conn.ID,
			ForgeProjectID:    p.ForgeProjectID,
			PathWithNamespace: p.PathWithNamespace,
			WebUrl:            p.WebURL,
			DefaultBranch:     branch,
		}); err != nil {
			slog.Error("upsert repo", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	repos, err := h.q.ListReposByConnectionForUser(r.Context(), store.ListReposByConnectionForUserParams{ConnectionID: conn.ID, UserID: user.ID})
	if err != nil {
		slog.Error("list repos", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]repoDTO, 0, len(repos))
	for _, rp := range repos {
		out = append(out, repoToDTO(rp))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"repos": out})
}

// ── Repos ───────────────────────────────────────────────────────────────────

// ListRepos returns the current user's enabled repos (sidebar picker).
func (h *Handler) ListRepos(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	repos, err := h.q.ListEnabledReposForUser(r.Context(), user.ID)
	if err != nil {
		slog.Error("list enabled repos", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]repoDTO, 0, len(repos))
	for _, rp := range repos {
		out = append(out, repoToDTO(rp))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"repos": out})
}

type setRepoEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

// SetRepoEnabled toggles whether a repo is tracked (its board shown, its poller
// active). Authorization is enforced in the UPDATE (user must own the
// connection); a non-owned or unknown id returns 404.
func (h *Handler) SetRepoEnabled(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid repo id")
		return
	}
	var req setRepoEnabledRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	repo, err := h.q.SetRepoEnabledForUser(r.Context(), store.SetRepoEnabledForUserParams{ID: id, Enabled: req.Enabled, UserID: user.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return
		}
		slog.Error("set repo enabled", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"repo": repoToDTO(repo)})
}
