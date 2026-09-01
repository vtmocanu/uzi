package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/toolprofile"
	"github.com/vtmocanu/uzi/api/internal/toolseed"
)

// maxProfilePackages bounds a repo tool profile's package list.
const maxProfilePackages = 64

// loadToolRules projects the DB allowlist into a toolprofile.Rules map for
// write-time package validation, via the shared loader (identical to the
// claim-time loader in workersvc, so save and claim can never diverge).
func (h *Handler) loadToolRules(r *http.Request) (toolprofile.Rules, error) {
	rows, err := h.q.ListToolAllowlist(r.Context())
	if err != nil {
		return nil, err
	}
	return toolprofile.RulesFromRows(rows), nil
}

// GetRepoToolProfile returns the caller's tool packages for a repo they own
// (PRD #18 M4). Owner-only: a non-owned or unknown repo id is 404. No profile yet
// ⇒ an empty list (not 404), so the UI can render the picker for a fresh repo.
func (h *Handler) GetRepoToolProfile(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "repo")
	if !ok {
		return
	}
	// Ownership gate: the repo must belong to the caller's connection.
	if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return
		}
		slog.Error("get repo for tool profile", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	packages := []string{}
	profile, err := h.q.GetRepoToolProfile(r.Context(), store.GetRepoToolProfileParams{UserID: user.ID, RepoID: id})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("get repo tool profile", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err == nil {
		if decoded := decodeProfilePackages(profile.Packages); decoded != nil {
			packages = decoded
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"packages": packages})
}

// SetRepoToolProfile replaces the caller's tool packages for a repo they own
// (PRD #18 M4). Every package is validated against the current allowlist at write
// time; an out-of-list or malformed entry rejects the whole save with the offending
// names (Success Criteria bullet 5). Owner-only: a non-owned/unknown repo is 404.
func (h *Handler) SetRepoToolProfile(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "repo")
	if !ok {
		return
	}
	var req struct {
		Packages []string `json:"packages"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Packages) > maxProfilePackages {
		httpx.Error(w, http.StatusBadRequest, fmt.Sprintf("a profile may list at most %d packages", maxProfilePackages))
		return
	}

	rules, err := h.loadToolRules(r)
	if err != nil {
		slog.Error("load tool allowlist", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	allowed, rejected := toolprofile.Resolve(req.Packages, rules)
	if len(rejected) > 0 {
		httpx.Error(w, http.StatusBadRequest, "these packages are not on the allowlist: "+strings.Join(rejected, ", "))
		return
	}
	// PRD #123 M3 (Decision 4c): even an allowlisted package must be in the baked
	// worker toolchain, or the run that requests it hangs then fails behind the
	// egress block. This second pass catches grandfathered allowlist rows a profile
	// still requests (rows that predate the CreateToolAllowlistEntry gate).
	var unbaked []string
	for _, p := range allowed {
		if !toolseed.Covered(p) {
			unbaked = append(unbaked, p)
		}
	}
	if len(unbaked) > 0 {
		httpx.Error(w, http.StatusBadRequest, "these packages are allowlisted but not in the baked worker toolchain (an image roll is required): "+strings.Join(unbaked, ", "))
		return
	}
	if allowed == nil {
		allowed = []string{}
	}
	raw, err := json.Marshal(allowed)
	if err != nil {
		slog.Error("marshal tool packages", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	profile, err := h.q.UpsertRepoToolProfileForOwner(r.Context(), store.UpsertRepoToolProfileForOwnerParams{
		UserID:   user.ID,
		RepoID:   id,
		Packages: raw,
	})
	if err != nil {
		// No row written ⇒ the repo is not owned by the caller (the ownership join
		// matched nothing). Hide existence: 404, not 403.
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return
		}
		slog.Error("upsert repo tool profile", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"packages": decodeProfilePackages(profile.Packages)})
}

// decodeProfilePackages decodes a packages JSONB column into a slice; a
// NULL/empty/malformed value yields an empty (non-nil) slice for the DTO.
func decodeProfilePackages(raw []byte) []string {
	out := []string{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return []string{}
	}
	return out
}
