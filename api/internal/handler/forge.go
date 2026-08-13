package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/privcheck"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// ── DTOs ────────────────────────────────────────────────────────────────────

type connectionDTO struct {
	ID             string `json:"id"`
	ForgeType      string `json:"forge_type"`
	BaseURL        string `json:"base_url"`
	BotUsername    string `json:"bot_username"`
	BotForgeUserID int64  `json:"bot_forge_user_id"`
	// HumanUsername is the owning user's own forge account, used for autopilot
	// attribution (PRD #19 M3). Null until the user declares it; distinct from the
	// bot identity above.
	HumanUsername  *string    `json:"human_username"`
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
	if c.HumanUsername.Valid {
		s := c.HumanUsername.String
		dto.HumanUsername = &s
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
		} else {
			// D7: rows written before PRD #65 hold "role" as a number, which no
			// longer unmarshals against the Role string field. The report blanks
			// until the next privilege sweep (UZI_PRIVILEGE_CHECK_INTERVAL, default
			// 24h) re-stamps it in the new shape. This log is deliberate — the
			// pre-#65 code discarded the error, which would hide a real corruption
			// behind the same silent blank as this expected one-time migration miss.
			slog.Warn("forge connection privilege report failed to unmarshal; blanking until the next privilege sweep re-stamps it",
				"connection", c.ID, "error", err)
		}
	}
	return dto
}

// repoDTO (apitypes.RepoDTO) moved to the stdlib-only apitypes leaf (PRD #64 M1);
// repoToDTO stays here as the store→DTO mapper.
func repoToDTO(r store.Repo) apitypes.RepoDTO {
	dto := apitypes.RepoDTO{
		ID:                  r.ID.String(),
		ConnectionID:        r.ConnectionID.String(),
		ForgeProjectID:      r.ForgeProjectID,
		PathWithNamespace:   r.PathWithNamespace,
		WebURL:              r.WebUrl,
		Enabled:             r.Enabled,
		RepoSkillsEnabled:   r.RepoSkillsEnabled,
		RepoClaudemdEnabled: r.RepoClaudemdEnabled,
		RepoDevboxOptIn:     r.RepoDevboxOptIn,
	}
	if r.DefaultBranch.Valid {
		dto.DefaultBranch = &r.DefaultBranch.String
	}
	// #66 M8 (D8): expose the admin per-repo override metadata when active. The
	// reason NULL is the discriminator — a non-NULL reason means the override is on.
	// Display-only surfacing for M9's badge; no findings downgrade happens here.
	if r.GuardrailOverrideReason.Valid {
		ov := &apitypes.GuardrailOverrideDTO{Reason: r.GuardrailOverrideReason.String}
		if r.GuardrailOverrideBy.Valid {
			ov.By = uuid.UUID(r.GuardrailOverrideBy.Bytes).String()
		}
		if r.GuardrailOverrideAt.Valid {
			ov.At = r.GuardrailOverrideAt.Time
		}
		dto.GuardrailOverride = ov
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
		"forge_types":       []string{string(forge.TypeGitLab), string(forge.TypeForgejo), string(forge.TypeGitHub)},
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
	if forgeType != string(forge.TypeGitLab) && forgeType != string(forge.TypeForgejo) && forgeType != string(forge.TypeGitHub) {
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
	if tr := h.pcheck.CheckToken(r.Context(), f, forge.Type(forgeType), identity.IsAdmin); len(tr.Violations) > 0 {
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

type updateConnectionRequest struct {
	HumanUsername string `json:"human_username"`
}

// maxHumanUsernameLen bounds the self-declared username; GitLab usernames top out
// well under this, so it only fences off absurd input written to a TEXT column.
const maxHumanUsernameLen = 255

const (
	// usernameNotFoundWarning is surfaced when the forge has no such account. The
	// value is still saved (verified-or-warned, PRD #19 Decision 3) — a warning,
	// not a hard reject, because a user may connect before their account is visible
	// to the bot, and hard-failing would be worse than a stored typo.
	usernameNotFoundWarning = "Saved, but no forge account with this username was found — double-check it matches your own forge username."
	// usernameUnverifiedWarning is surfaced when the lookup itself fails (forge
	// unreachable, rate-limited). The save is not blocked on our ability to verify.
	usernameUnverifiedWarning = "Saved, but the username could not be verified against the forge right now."
)

// UpdateConnection edits a connection's mutable fields — today only
// human_username, the owning user's own forge account used for autopilot
// attribution (PRD #19 M3). Saving is verified-or-warned: a value that does not
// resolve to a forge user is still stored, with a warning, never hard-rejected
// (Decision 3 — identity is self-declared). A value another uzi user has already
// mapped on the same host IS hard-rejected (409) by the partial unique index. An
// empty value clears the mapping (and skips the forge round-trip).
func (h *Handler) UpdateConnection(w http.ResponseWriter, r *http.Request) {
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
	var req updateConnectionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	username := strings.TrimSpace(req.HumanUsername)
	if utf8.RuneCountInString(username) > maxHumanUsernameLen {
		httpx.Error(w, http.StatusBadRequest, "username is too long")
		return
	}

	conn, err := h.q.GetForgeConnectionForUser(r.Context(), store.GetForgeConnectionForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "connection not found")
		return
	}

	// Best-effort verify BEFORE the write so the warning describes exactly the
	// value we are about to store. Skip the round-trip entirely when clearing.
	var warning string
	if username != "" {
		f, ferr := h.svc.ForgeForConnection(conn.ForgeType, conn.BaseUrl, conn.TokenCiphertext)
		if ferr != nil {
			slog.Error("build forge for connection", "error", ferr)
			warning = usernameUnverifiedWarning
		} else {
			warning = humanUsernameWarning(r.Context(), f, username)
		}
	}

	updated, err := h.q.SetForgeConnectionHumanUsername(r.Context(), store.SetForgeConnectionHumanUsernameParams{
		ID:            id,
		UserID:        user.ID,
		HumanUsername: pgtypeTextOrNull(username),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Error(w, http.StatusConflict, "that forge username is already mapped by another user on this host")
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			// The connection was deleted between the read above and this write.
			httpx.Error(w, http.StatusNotFound, "connection not found")
			return
		}
		slog.Error("set connection human username", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	resp := map[string]any{"connection": connToDTO(updated)}
	if warning != "" {
		resp["warning"] = warning
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// humanUsernameWarning best-effort confirms username resolves to a forge account.
// It returns "" when the user exists, and a warning string when the forge says no
// such user or the lookup itself fails — a forge blip must never block the save
// (PRD #19 Decision 3, verified-or-warned).
func humanUsernameWarning(ctx context.Context, f forge.Forge, username string) string {
	exists, err := f.UserExists(ctx, username)
	if err != nil {
		return usernameUnverifiedWarning
	}
	if !exists {
		return usernameNotFoundWarning
	}
	return ""
}

// PrivilegeCheck runs the full PAT least-privilege report for a connection
// (token + every enabled repo), persists it, and returns it. Owner-only, behind
// the per-user forge rate limiter (it is the heaviest forge-proxying route: two
// token-level calls — VerifyToken + TokenInfo — plus two per enabled repo). It
// never blocks — the report surfaces findings, the badge reflects them, and the
// user acts.
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
	out := make([]apitypes.RepoDTO, 0, len(repos))
	repoIDs := make([]uuid.UUID, len(repos))
	for i, rp := range repos {
		repoIDs[i] = rp.ID
	}
	pipelines, err := h.defaultBranchPipelines(r.Context(), repoIDs)
	if err != nil {
		// Non-fatal: badges are enrichment, not the payload. Render without them.
		slog.Warn("list projects: default-branch pipelines", "error", err)
	}
	for _, rp := range repos {
		d := repoToDTO(rp)
		d.Pipeline = pipelines[rp.ID]
		out = append(out, d)
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
	out := make([]apitypes.RepoDTO, 0, len(repos))
	repoIDs := make([]uuid.UUID, len(repos))
	for i, rp := range repos {
		repoIDs[i] = rp.ID
	}
	pipelines, err := h.defaultBranchPipelines(r.Context(), repoIDs)
	if err != nil {
		slog.Warn("list repos: default-branch pipelines", "error", err)
	}
	for _, rp := range repos {
		d := repoToDTO(rp)
		d.Pipeline = pipelines[rp.ID]
		out = append(out, d)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"repos": out})
}

type setRepoEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

// blockFindingMessages extracts the human messages of exactly the SeverityBlock
// findings, for the 422 "violations" array (PRD #66 D1 layer 1). Overridden and
// warn findings are excluded — only findings that actually refuse the run are
// reported as the reason it was refused.
func blockFindingMessages(findings []privcheck.Finding) []string {
	msgs := make([]string, 0, len(findings))
	for _, f := range findings {
		if f.Severity == privcheck.SeverityBlock {
			msgs = append(msgs, f.Message)
		}
	}
	return msgs
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

	// D1 layer 1 (PRD #66): the repo-enable gate. Enabling is the moment the user
	// is present and can fix a forge misconfiguration, so a live, fail-closed guard
	// runs BEFORE the flip on the enable path only. The disable path is NEVER gated
	// (D4) — a user must always be able to stop tracking a repo.
	if req.Enabled {
		row, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusNotFound, "repo not found")
				return
			}
			slog.Error("get repo for enable guard", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		res := h.pcheck.GuardRepo(r.Context(), privcheck.GuardInput{
			ForgeType:       row.ForgeType,
			BaseURL:         row.BaseUrl,
			TokenCiphertext: row.TokenCiphertext,
			Repo: privcheck.Repo{
				ID:             row.ID.String(),
				Path:           row.PathWithNamespace,
				ForgeProjectID: row.ForgeProjectID,
				DefaultBranch:  row.DefaultBranch.String,
			},
			// Live per-repo override (M8): a non-NULL guardrail_override_reason means
			// the admin override is active, so GuardRepo downgrades the waivable
			// findings post-evaluation — never protection_unreadable (D8/D3).
			Overridden: row.GuardrailOverrideReason.Valid,
		})
		if res.Blocked {
			// 422 mirroring the save-time token gate's body shape (forge.go, key
			// "violations") so the existing web 422 handling applies. Only the
			// SeverityBlock findings' messages go in "violations" — an overridden or
			// warn finding must not appear as a reason the run was refused.
			httpx.JSON(w, http.StatusUnprocessableEntity, map[string]any{
				// Headline stays cause-agnostic: the block set spans both "the bot
				// can push/merge to the default branch" and the fail-closed
				// "protection could not be verified" case, and hardcoding the
				// push/merge reason misdescribes the latter. The specific, actionable
				// reason(s) are in "violations".
				"error":      "this repo cannot be enabled: uzi will not run while the bot can reach the default branch, or while that cannot be verified (main is never touched). See the reasons below, fix branch protection on the forge, then retry.",
				"violations": blockFindingMessages(res.Findings),
			})
			return
		}
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

type patchRepoRequest struct {
	// RepoSkillsEnabled is the repo-skills opt-in (PRD #16): load skills from the
	// repo's own .claude/skills at run time. Pointer so an omitted field is a
	// no-op rather than a silent disable.
	RepoSkillsEnabled *bool `json:"repo_skills_enabled"`
	// RepoClaudemdEnabled is the trusted-repo instructions opt-in (PRD #246): let the
	// lead read the clone's root CLAUDE.md as advisory context. Pointer so an omitted
	// field is a no-op. A sibling trust flag of RepoSkillsEnabled — the two may be set
	// together or individually in one request (the "Trusted repo" master).
	RepoClaudemdEnabled *bool `json:"repo_claudemd_enabled"`
	// RepoDevboxOptIn is the tier-2 opt-in (PRD #18 M5): union the repo's own
	// devbox.json packages (packages-only) into provisioning. Pointer = omitted is a
	// no-op. Its own exclusive path — cannot be combined with the trust flags.
	RepoDevboxOptIn *bool `json:"repo_devbox_opt_in"`
}

// optBoolToPgtype maps an optional request bool to a pgtype.Bool: a nil pointer is
// an absent value (Valid:false), which the COALESCE in SetRepoTrustFlags reads as
// "leave this column unchanged"; a non-nil pointer is the value to set.
func optBoolToPgtype(v *bool) pgtype.Bool {
	if v == nil {
		return pgtype.Bool{Valid: false}
	}
	return pgtype.Bool{Bool: *v, Valid: true}
}

// PatchRepo updates a repo's mutable opt-in settings. Two disjoint paths: the
// trust flags (repo_skills_enabled and/or repo_claudemd_enabled, PRD #16/#246) —
// which may be set together or individually in one atomic round-trip
// (SetRepoTrustFlags) — and repo_devbox_opt_in (PRD #18), which is its own exclusive
// path. The two paths cannot be combined in one request. At least one field must be
// present. Authorization: the repo owner (via the owning connection) or an admin. A
// non-owned, unknown id returns 404 for a non-admin; an admin may target any repo.
func (h *Handler) PatchRepo(w http.ResponseWriter, r *http.Request) {
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
	var req patchRepoRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	devboxSet := req.RepoDevboxOptIn != nil
	trustSet := req.RepoSkillsEnabled != nil || req.RepoClaudemdEnabled != nil
	if devboxSet && trustSet {
		httpx.Error(w, http.StatusBadRequest, "repo_devbox_opt_in cannot be combined with repo_skills_enabled or repo_claudemd_enabled")
		return
	}
	if !devboxSet && !trustSet {
		httpx.Error(w, http.StatusBadRequest, "provide repo_devbox_opt_in, or at least one of repo_skills_enabled or repo_claudemd_enabled")
		return
	}

	var repo store.Repo
	switch {
	case trustSet:
		// One atomic round-trip sets both trust columns; a nil field is left unchanged
		// by the query's COALESCE, so the master toggle and each sub-toggle share this
		// path with no partial-failure window.
		skills := optBoolToPgtype(req.RepoSkillsEnabled)
		claudemd := optBoolToPgtype(req.RepoClaudemdEnabled)
		if user.IsAdmin {
			repo, err = h.q.SetRepoTrustFlags(r.Context(), store.SetRepoTrustFlagsParams{ID: id, Skills: skills, Claudemd: claudemd})
		} else {
			repo, err = h.q.SetRepoTrustFlagsForUser(r.Context(), store.SetRepoTrustFlagsForUserParams{ID: id, Skills: skills, Claudemd: claudemd, UserID: user.ID})
		}
	case devboxSet:
		if user.IsAdmin {
			repo, err = h.q.SetRepoDevboxOptIn(r.Context(), store.SetRepoDevboxOptInParams{ID: id, RepoDevboxOptIn: *req.RepoDevboxOptIn})
		} else {
			repo, err = h.q.SetRepoDevboxOptInForUser(r.Context(), store.SetRepoDevboxOptInForUserParams{ID: id, RepoDevboxOptIn: *req.RepoDevboxOptIn, UserID: user.ID})
		}
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return
		}
		slog.Error("patch repo settings", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"repo": repoToDTO(repo)})
}
