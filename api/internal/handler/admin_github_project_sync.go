package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// adoptGithubProjectSyncRequest is the POST body: which existing Projects v2 board
// to adopt for this repo. owner_kind selects the GraphQL owner root (viewer/user/
// org) the project number resolves under; it defaults to "user" (the common case:
// a project owned by the connection's user account). The repo target is taken from
// the path, never the body (audit).
type adoptGithubProjectSyncRequest struct {
	ProjectNumber int    `json:"project_number"`
	OwnerKind     string `json:"owner_kind"`
}

// parseOwnerKind maps the request's owner_kind string to a forge.ProjectV2OwnerKind.
// An empty value defaults to OwnerUser (the documented default). An unrecognised
// value is rejected so a typo does not silently resolve against the wrong root.
func parseOwnerKind(s string) (forge.ProjectV2OwnerKind, bool) {
	switch s {
	case "", "user":
		return forge.OwnerUser, true
	case "org":
		return forge.OwnerOrg, true
	case "viewer":
		return forge.OwnerViewer, true
	default:
		return 0, false
	}
}

// AdoptGithubProjectSync is the admin adopt/link write (PRD #364 M3): link an
// EXISTING GitHub Projects v2 board to this repo's label board and seed it. Mounted
// under the admin WRITE group (RequireAuth + RequireAdmin), so it is cookie-only and
// admin-only — the sync writes to a user's project board, an instance-admin decision.
// The actor is authorized by RequireAdmin and the repo target comes from the path;
// the body carries only the project coordinates.
func (h *Handler) AdoptGithubProjectSync(w http.ResponseWriter, r *http.Request) {
	if _, ok := mw.UserFromContext(r.Context()); !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid repo id")
		return
	}
	var req adoptGithubProjectSyncRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectNumber <= 0 {
		httpx.Error(w, http.StatusBadRequest, "project_number must be a positive integer")
		return
	}
	ownerKind, ok := parseOwnerKind(req.OwnerKind)
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "owner_kind must be one of user, org, viewer")
		return
	}
	if h.projectSync == nil {
		slog.Error("github project sync adopt: service not wired")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := h.projectSync.Adopt(r.Context(), id, req.ProjectNumber, ownerKind); err != nil {
		writeProjectSyncError(w, "adopt", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "linked"})
}

// provisionGithubProjectSyncRequest is the POST body for the autonomous-provision
// route (PRD #364 M4): owner_kind selects the GraphQL owner root the new project is
// created under (defaults to "user"), and title optionally names the created board
// (defaulting to a sensible value in the service when empty). The repo target is
// taken from the path, never the body (audit), like adopt.
type provisionGithubProjectSyncRequest struct {
	OwnerKind string `json:"owner_kind"`
	Title     string `json:"title"`
}

// ProvisionGithubProjectSync is the admin autonomous-provision write (PRD #364 M4):
// CREATE a GitHub Projects v2 board with uzi's OWN "uzi Status" field, link it to
// this repo, and seed it — zero manual GitHub-UI clicks. Same admin-only, path-scoped
// shape as the adopt route (RequireAuth + RequireAdmin), and the same error mapping;
// it persists owned_by_uzi=true. Success is 201 Created (a new board was made).
func (h *Handler) ProvisionGithubProjectSync(w http.ResponseWriter, r *http.Request) {
	if _, ok := mw.UserFromContext(r.Context()); !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid repo id")
		return
	}
	var req provisionGithubProjectSyncRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ownerKind, ok := parseOwnerKind(req.OwnerKind)
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "owner_kind must be one of user, org, viewer")
		return
	}
	if h.projectSync == nil {
		slog.Error("github project sync provision: service not wired")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := h.projectSync.Provision(r.Context(), id, ownerKind, req.Title); err != nil {
		writeProjectSyncError(w, "provision", err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"status": "provisioned"})
}

// DisableGithubProjectSync is the admin teardown (PRD #364 M3): drop the repo's
// project link + item projection rows. Same admin-only, path-scoped shape as the
// adopt route. It does not touch the project board itself (M7 refines that).
func (h *Handler) DisableGithubProjectSync(w http.ResponseWriter, r *http.Request) {
	if _, ok := mw.UserFromContext(r.Context()); !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid repo id")
		return
	}
	if h.projectSync == nil {
		slog.Error("github project sync disable: service not wired")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := h.projectSync.Disable(r.Context(), id); err != nil {
		writeProjectSyncError(w, "disable", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getGithubProjectSyncStatusResponse is the health-endpoint body (PRD #364 M7): the
// link's observability snapshot. last_synced_at/last_error are pointers so a
// never-synced or healthy link renders them as JSON null rather than a zero value.
type getGithubProjectSyncStatusResponse struct {
	ProjectNumber int64      `json:"project_number"`
	OwnedByUzi    bool       `json:"owned_by_uzi"`
	LastSyncedAt  *time.Time `json:"last_synced_at"`
	LastError     *string    `json:"last_error"`
	ItemCount     int        `json:"item_count"`
}

// GetGithubProjectSyncStatus is the admin sync-health read (PRD #364 M7): report a
// repo's project link status (project number, ownership, last_synced_at, last_error,
// item_count). Mounted under the admin READ group (RequireUser + RequireAdminRO) — it
// is a read of the stored projection, no forge call — unlike the adopt/disable writes.
// A repo with no link row is 404: "no link = not sync-enabled".
func (h *Handler) GetGithubProjectSyncStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := mw.UserFromContext(r.Context()); !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid repo id")
		return
	}
	if h.projectSync == nil {
		slog.Error("github project sync status: service not wired")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	status, err := h.projectSync.ProjectSyncStatus(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "project sync not enabled for this repo")
			return
		}
		slog.Error("github project sync status", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	resp := getGithubProjectSyncStatusResponse{
		ProjectNumber: status.ProjectNumber,
		OwnedByUzi:    status.OwnedByUzi,
		ItemCount:     status.ItemCount,
	}
	if status.LastSyncedAt.Valid {
		t := status.LastSyncedAt.Time
		resp.LastSyncedAt = &t
	}
	if status.LastError.Valid {
		e := status.LastError.String
		resp.LastError = &e
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// writeProjectSyncError maps a provisioning error to an HTTP status + clean body.
// The forgesvc sentinels are user-actionable preconditions (4xx); an unknown repo
// id is a 404; anything else is an internal error whose raw text is logged, not
// returned.
func writeProjectSyncError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "repo not found")
	case errors.Is(err, forgesvc.ErrProjectSyncDisabled):
		httpx.Error(w, http.StatusConflict, err.Error())
	case errors.Is(err, forgesvc.ErrProjectSyncNotGitHub),
		errors.Is(err, forgesvc.ErrProjectSyncUnsupported),
		errors.Is(err, forgesvc.ErrProjectSyncMissingScope):
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
	default:
		slog.Error("github project sync "+op, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}
