package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
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

// AdoptGithubProjectSync is the adopt/link write (PRD #364 M3, relocated to
// owner-or-admin by issue #534 D4): link an EXISTING GitHub Projects v2 board to this
// repo's label board and seed it. Mounted under the per-repo RequireAuth group, so it
// is cookie-only; authorization is owner-or-admin — a non-admin must own the repo
// (GetRepoForUser preflight below, 404 for a foreign/unknown id), while an admin skips
// the preflight. The repo target comes from the path; the body carries only the
// project coordinates.
func (h *Handler) AdoptGithubProjectSync(w http.ResponseWriter, r *http.Request) {
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
	if !user.IsAdmin {
		if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusNotFound, "repo not found")
				return
			}
			slog.Error("github project sync: owner preflight", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
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

// ResyncGithubProjectSync re-runs the adopt seed against a repo's already-linked board
// (PRD #576 M3): it re-reads the Status field so newly-added options are picked up and
// re-persists the recomputed unmatched set, giving the user a one-click fix loop after
// they add the missing Status options in GitHub. Same owner-or-admin, path-scoped shape
// and nil-guard as the sibling routes; it needs NO body (the coordinates come from the
// stored link). A repo with no link row maps to 404 ("not linked") via the not-linked
// sentinel. Success is 200 {"status":"resynced"}.
func (h *Handler) ResyncGithubProjectSync(w http.ResponseWriter, r *http.Request) {
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
	if !user.IsAdmin {
		if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusNotFound, "repo not found")
				return
			}
			slog.Error("github project sync: owner preflight", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if h.projectSync == nil {
		slog.Error("github project sync resync: service not wired")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := h.projectSync.Resync(r.Context(), id); err != nil {
		writeProjectSyncError(w, "resync", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "resynced"})
}

// AutoCreateGithubProjectColumns is the safe column auto-create write (PRD #576 M6):
// create a FRESH uzi-owned single-select field on the adopted board carrying all of the
// repo's board columns, switch the link to it, and re-seed — turning skipped columns into
// synced ones with no manual GitHub edit and no destructive field replace. Same
// owner-or-admin, path-scoped shape and nil-guard as the sibling routes; it needs NO body
// (the coordinates come from the stored link). A repo with no link row maps to 404 ("not
// linked") via the not-linked sentinel. Success is 200 {"status":"columns_created"}.
func (h *Handler) AutoCreateGithubProjectColumns(w http.ResponseWriter, r *http.Request) {
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
	if !user.IsAdmin {
		if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusNotFound, "repo not found")
				return
			}
			slog.Error("github project sync: owner preflight", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if h.projectSync == nil {
		slog.Error("github project sync autocreate-columns: service not wired")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := h.projectSync.AutoCreateColumns(r.Context(), id); err != nil {
		writeProjectSyncError(w, "autocreate-columns", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "columns_created"})
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

// ProvisionGithubProjectSync is the autonomous-provision write (PRD #364 M4,
// owner-or-admin by issue #534 D4): CREATE a GitHub Projects v2 board with uzi's OWN
// "uzi Status" field, link it to this repo, and seed it — zero manual GitHub-UI
// clicks. Same owner-or-admin, path-scoped shape as the adopt route, and the same
// error mapping; it persists owned_by_uzi=true. Success is 201 Created (a new board
// was made).
func (h *Handler) ProvisionGithubProjectSync(w http.ResponseWriter, r *http.Request) {
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
	if !user.IsAdmin {
		if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusNotFound, "repo not found")
				return
			}
			slog.Error("github project sync: owner preflight", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
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

// DisableGithubProjectSync is the teardown (PRD #364 M3, owner-or-admin by issue #534
// D4): drop the repo's project link + item projection rows. Same owner-or-admin,
// path-scoped shape as the adopt route. It does not touch the project board itself
// (M7 refines that).
func (h *Handler) DisableGithubProjectSync(w http.ResponseWriter, r *http.Request) {
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
	if !user.IsAdmin {
		if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusNotFound, "repo not found")
				return
			}
			slog.Error("github project sync: owner preflight", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
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
	// UnmatchedColumns are board columns with no matching Status option at the last
	// adopt/resync (PRD #576 M3); the panel surfaces them with a Resync prompt. Always
	// a JSON array (never null) so the frontend can `.length` it unconditionally.
	UnmatchedColumns []string `json:"unmatched_columns"`
}

// GetGithubProjectSyncStatus is the sync-health read (PRD #364 M7, owner-or-admin by
// issue #534 D4): report a repo's project link status (project number, ownership,
// last_synced_at, last_error, item_count). Mounted under the per-repo RequireAuth
// group — it is a read of the stored projection, no forge call. Authorization is
// owner-or-admin: a non-admin must own the repo (GetRepoForUser preflight, 404 for a
// foreign/unknown id), while an admin skips it. A repo with no link row is a
// (different) 404: "no link = not sync-enabled".
func (h *Handler) GetGithubProjectSyncStatus(w http.ResponseWriter, r *http.Request) {
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
	if !user.IsAdmin {
		if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusNotFound, "repo not found")
				return
			}
			slog.Error("github project sync: owner preflight", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
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
	unmatched := status.UnmatchedColumns
	if unmatched == nil {
		unmatched = []string{}
	}
	resp := getGithubProjectSyncStatusResponse{
		ProjectNumber:    status.ProjectNumber,
		OwnedByUzi:       status.OwnedByUzi,
		ItemCount:        status.ItemCount,
		UnmatchedColumns: unmatched,
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

// GetGithubProjectOwnerType is the owner-type read (PRD #576 M1, owner-or-admin by
// issue #534 D4): report whether the repo's GitHub owner is a "User" or an
// "Organization", for the sync panel's Provision feasibility nudge. Like the
// visibility read it makes a live forge round-trip (repositoryOwner __typename), and
// unlike the DB-only status route it needs NO link row — it is fetched for a
// not-yet-linked repo. Same owner-or-admin, path-scoped shape and error mapping as the
// sibling routes: a non-admin must own the repo (GetRepoForUser preflight, 404 for a
// foreign/unknown id), an admin skips it; the preflight runs BEFORE the nil-guard.
func (h *Handler) GetGithubProjectOwnerType(w http.ResponseWriter, r *http.Request) {
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
	if !user.IsAdmin {
		if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusNotFound, "repo not found")
				return
			}
			slog.Error("github project sync: owner preflight", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if h.projectSync == nil {
		slog.Error("github project sync owner-type: service not wired")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	ot, err := h.projectSync.RepoOwnerType(r.Context(), id)
	if err != nil {
		writeProjectSyncError(w, "owner-type", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"owner_type": string(ot)})
}

// setVisibilityRequest is the PUT body for the visibility toggle: the desired public
// flag. The repo target is taken from the path, never the body (audit), like adopt.
type setVisibilityRequest struct {
	Public bool `json:"public"`
}

// collaboratorRequest is the POST/DELETE body for the share/unshare routes: the GitHub
// login to grant/revoke Reader access. Reused for both because the shape is identical;
// the role is fixed to Reader in v1 (PRD #557 D3), so there is no role field. The repo
// target is taken from the path, never the body (audit).
type collaboratorRequest struct {
	Username string `json:"username"`
}

// GetGithubProjectVisibility is the visibility read (PRD #557 M3, owner-or-admin by
// issue #534 D4): report the linked board's current public flag. Unlike the DB-only
// status route it makes a live forge round-trip (ProjectV2.public), so it is a
// separate lazy GET issued only when the Board-access section opens (D4). Same
// owner-or-admin, path-scoped shape as the status route: a non-admin must own the repo
// (GetRepoForUser preflight, 404 for a foreign/unknown id), an admin skips it. The
// preflight runs BEFORE the nil-guard so a non-owner is 404'd regardless.
func (h *Handler) GetGithubProjectVisibility(w http.ResponseWriter, r *http.Request) {
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
	if !user.IsAdmin {
		if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusNotFound, "repo not found")
				return
			}
			slog.Error("github project sync: owner preflight", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if h.projectSync == nil {
		slog.Error("github project sync visibility: service not wired")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	public, err := h.projectSync.GetVisibility(r.Context(), id)
	if err != nil {
		writeProjectSyncError(w, "visibility", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"public": public})
}

// SetGithubProjectVisibility is the visibility write (PRD #557 M3, owner-or-admin):
// flip the linked board's public flag through updateProjectV2. Same owner-or-admin,
// path-scoped shape as the status route; the body carries only the desired flag. The
// owner-or-admin preflight runs BEFORE the body decode and BEFORE the nil-guard, so a
// non-owner is 404'd even with an empty/absent body.
func (h *Handler) SetGithubProjectVisibility(w http.ResponseWriter, r *http.Request) {
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
	if !user.IsAdmin {
		if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusNotFound, "repo not found")
				return
			}
			slog.Error("github project sync: owner preflight", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	var req setVisibilityRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if h.projectSync == nil {
		slog.Error("github project sync visibility: service not wired")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := h.projectSync.SetVisibility(r.Context(), id, req.Public); err != nil {
		writeProjectSyncError(w, "visibility", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"public": req.Public})
}

// ShareGithubProjectSync is the grant-Reader write (PRD #557 M3, owner-or-admin):
// grant the named GitHub login Reader access to the linked board via
// updateProjectV2Collaborators. This is WRITE-ONLY by necessity (D2): GitHub's
// ProjectV2 exposes no readable collaborators field, so uzi grants/revokes but never
// enumerates the current list. A non-existent login surfaces as
// ErrProjectSyncUserNotFound → 422 (not a 500). Same owner-or-admin, path-scoped shape
// as the status route; the preflight runs BEFORE body decode and the nil-guard.
func (h *Handler) ShareGithubProjectSync(w http.ResponseWriter, r *http.Request) {
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
	if !user.IsAdmin {
		if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusNotFound, "repo not found")
				return
			}
			slog.Error("github project sync: owner preflight", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	var req collaboratorRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		httpx.Error(w, http.StatusBadRequest, "username is required")
		return
	}
	if h.projectSync == nil {
		slog.Error("github project sync share: service not wired")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := h.projectSync.ShareWithUser(r.Context(), id, req.Username); err != nil {
		writeProjectSyncError(w, "share", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// UnshareGithubProjectSync is the revoke write (PRD #557 M3, owner-or-admin): set the
// named GitHub login's role back to none via updateProjectV2Collaborators. Reuses the
// same collaboratorRequest body as share (a DELETE with a JSON body — chi/net-http
// reads it fine). Same owner-or-admin, path-scoped shape; the preflight runs BEFORE
// body decode and the nil-guard.
func (h *Handler) UnshareGithubProjectSync(w http.ResponseWriter, r *http.Request) {
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
	if !user.IsAdmin {
		if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusNotFound, "repo not found")
				return
			}
			slog.Error("github project sync: owner preflight", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	var req collaboratorRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		httpx.Error(w, http.StatusBadRequest, "username is required")
		return
	}
	if h.projectSync == nil {
		slog.Error("github project sync unshare: service not wired")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := h.projectSync.Unshare(r.Context(), id, req.Username); err != nil {
		writeProjectSyncError(w, "unshare", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeProjectSyncError maps a provisioning error to an HTTP status + clean body.
// The forgesvc sentinels are user-actionable preconditions (4xx); an unknown repo
// id is a 404; anything else is an internal error whose raw text is logged, not
// returned.
func writeProjectSyncError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "repo not found")
	case errors.Is(err, forgesvc.ErrProjectSyncNotLinked):
		httpx.Error(w, http.StatusNotFound, err.Error())
	case errors.Is(err, forgesvc.ErrProjectSyncDisabled),
		errors.Is(err, forgesvc.ErrProjectSyncAlreadyLinked):
		httpx.Error(w, http.StatusConflict, err.Error())
	case errors.Is(err, forgesvc.ErrProjectSyncNotGitHub),
		errors.Is(err, forgesvc.ErrProjectSyncUnsupported),
		errors.Is(err, forgesvc.ErrProjectSyncMissingScope),
		errors.Is(err, forgesvc.ErrProjectSyncUserNotFound):
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
	default:
		slog.Error("github project sync "+op, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}
