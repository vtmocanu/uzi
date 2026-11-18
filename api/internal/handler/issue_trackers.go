package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// issueTrackerConnectionDTO is the wire form of a connection. It has no token or
// ciphertext field, like issuetrackersvc.Connection.
type issueTrackerConnectionDTO struct {
	ID             uuid.UUID `json:"id"`
	Kind           string    `json:"kind"`
	BaseURL        string    `json:"base_url"`
	AccountID      string    `json:"account_id"`
	AccountLogin   string    `json:"account_login"`
	CreatedAt      time.Time `json:"created_at"`
	LastVerifiedAt time.Time `json:"last_verified_at"`
}

// cleanTrackerText drops control, bidirectional, zero-width and other invisible characters
// from text that came from the issue tracker, so a hostile name cannot reorder or hide
// text in the browser. The predicate is the one the driver and the service already use.
func cleanTrackerText(s string) string {
	return strings.Map(func(r rune) rune {
		if issuetracker.InvisibleRune(r) {
			return -1
		}
		return r
	}, s)
}

func toIssueTrackerConnectionDTO(c issuetrackersvc.Connection) issueTrackerConnectionDTO {
	return issueTrackerConnectionDTO{
		ID: c.ID, Kind: c.Kind, BaseURL: c.BaseURL, AccountID: c.AccountID,
		AccountLogin: c.AccountLogin, CreatedAt: c.CreatedAt, LastVerifiedAt: c.LastVerifiedAt,
	}
}

type issueTrackerAccountDTO struct {
	ID    string `json:"id"`
	Login string `json:"login"`
}

type issueTrackerGroupDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	AllUsers   bool   `json:"all_users"`
	UsersCount int    `json:"users_count"`
}

type issueTrackerSharingDTO struct {
	Groups []issueTrackerGroupDTO   `json:"groups"`
	Users  []issueTrackerAccountDTO `json:"users"`
}

func toIssueTrackerSharingDTO(s issuetracker.SharingSettings) issueTrackerSharingDTO {
	out := issueTrackerSharingDTO{
		Groups: make([]issueTrackerGroupDTO, 0, len(s.Groups)),
		Users:  make([]issueTrackerAccountDTO, 0, len(s.Users)),
	}
	for _, g := range s.Groups {
		out.Groups = append(out.Groups, issueTrackerGroupDTO{ID: cleanTrackerText(g.ID), Name: cleanTrackerText(g.Name), AllUsers: g.AllUsers, UsersCount: g.UsersCount})
	}
	for _, u := range s.Users {
		out.Users = append(out.Users, issueTrackerAccountDTO{ID: cleanTrackerText(u.ID), Login: cleanTrackerText(u.Login)})
	}
	return out
}

type issueTrackerProjectDTO struct {
	ID        string `json:"id"`
	ShortName string `json:"short_name"`
	Name      string `json:"name"`
}

// issueTrackerTagDTO is a tag with the two sharing verdicts a link requires, judged for
// the connection's account. The verdicts speak about who can apply the tag only; they do
// not say who can edit issues in a project.
type issueTrackerTagDTO struct {
	ID              string                 `json:"id"`
	Name            string                 `json:"name"`
	Owner           issueTrackerAccountDTO `json:"owner"`
	Read            issueTrackerSharingDTO `json:"read"`
	Use             issueTrackerSharingDTO `json:"use"`
	AccountCanApply bool                   `json:"account_can_apply"`
	OthersCanApply  bool                   `json:"others_can_apply"`
}

type issueTrackerLinkDTO struct {
	RepoID           uuid.UUID `json:"repo_id"`
	ConnectionID     uuid.UUID `json:"connection_id"`
	ProjectID        string    `json:"project_id"`
	ProjectShortName string    `json:"project_short_name"`
	TagID            string    `json:"tag_id"`
	TagName          string    `json:"tag_name"`
	LinkedAt         time.Time `json:"linked_at"`
}

type createIssueTrackerConnectionRequest struct {
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
}

type linkRepoToIssueTrackerRequest struct {
	ConnectionID string `json:"connection_id"`
	ProjectID    string `json:"project_id"`
	TagID        string `json:"tag_id"`
}

// issueTrackerRequest returns the signed-in user and the service, or writes the refusal:
// 401 without a user, 503 when no service is wired.
func (h *Handler) issueTrackerRequest(w http.ResponseWriter, r *http.Request) (uuid.UUID, *issuetrackersvc.Service, bool) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return uuid.Nil, nil, false
	}
	if h.issueTrackers == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "issue trackers are not configured")
		return uuid.Nil, nil, false
	}
	return user.ID, h.issueTrackers, true
}

// writeIssueTrackerError maps an issuetrackersvc failure to its status. Messages are
// already safe to show: tracker text is token-redacted by the driver, and an internal
// failure carries a fixed message.
func writeIssueTrackerError(w http.ResponseWriter, err error) {
	var e *issuetrackersvc.Error
	if !errors.As(err, &e) {
		slog.Error("issue tracker", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	switch e.Kind {
	case issuetrackersvc.KindBadRequest:
		httpx.Error(w, http.StatusBadRequest, e.Message)
	case issuetrackersvc.KindNotFound:
		httpx.Error(w, http.StatusNotFound, e.Message)
	case issuetrackersvc.KindConflict:
		httpx.Error(w, http.StatusConflict, e.Message)
	case issuetrackersvc.KindBadGateway:
		httpx.Error(w, http.StatusBadGateway, e.Message)
	default:
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

// CreateIssueTrackerConnection verifies a token against the issue tracker and stores it
// (POST /api/issue-trackers/connections). With the issue tracker allowlist empty every
// base URL is refused.
func (h *Handler) CreateIssueTrackerConnection(w http.ResponseWriter, r *http.Request) {
	userID, svc, ok := h.issueTrackerRequest(w, r)
	if !ok {
		return
	}
	var req createIssueTrackerConnectionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	conn, err := svc.Connect(r.Context(), userID, req.Kind, req.BaseURL, req.Token)
	if err != nil {
		writeIssueTrackerError(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"connection": toIssueTrackerConnectionDTO(conn)})
}

// ListIssueTrackerConnections lists the user's issue tracker connections.
func (h *Handler) ListIssueTrackerConnections(w http.ResponseWriter, r *http.Request) {
	userID, svc, ok := h.issueTrackerRequest(w, r)
	if !ok {
		return
	}
	conns, err := svc.List(r.Context(), userID)
	if err != nil {
		writeIssueTrackerError(w, err)
		return
	}
	out := make([]issueTrackerConnectionDTO, 0, len(conns))
	for _, c := range conns {
		out = append(out, toIssueTrackerConnectionDTO(c))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"connections": out})
}

// DeleteIssueTrackerConnection deletes a connection; 409 while a repo is linked to it.
func (h *Handler) DeleteIssueTrackerConnection(w http.ResponseWriter, r *http.Request) {
	userID, svc, ok := h.issueTrackerRequest(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "connection")
	if !ok {
		return
	}
	if err := svc.Delete(r.Context(), userID, id); err != nil {
		writeIssueTrackerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListIssueTrackerProjects lists the projects the connection's token can see.
func (h *Handler) ListIssueTrackerProjects(w http.ResponseWriter, r *http.Request) {
	userID, svc, ok := h.issueTrackerRequest(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "connection")
	if !ok {
		return
	}
	projects, err := svc.ListProjects(r.Context(), userID, id)
	if err != nil {
		writeIssueTrackerError(w, err)
		return
	}
	out := make([]issueTrackerProjectDTO, 0, len(projects))
	for _, p := range projects {
		out = append(out, issueTrackerProjectDTO{ID: cleanTrackerText(p.ID), ShortName: cleanTrackerText(p.ShortName), Name: cleanTrackerText(p.Name)})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"projects": out})
}

// ListIssueTrackerTags lists the tags the connection's token can see, each with its
// owner, sharing settings and whether the connection's account (and anyone else) can
// apply it.
func (h *Handler) ListIssueTrackerTags(w http.ResponseWriter, r *http.Request) {
	userID, svc, ok := h.issueTrackerRequest(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "connection")
	if !ok {
		return
	}
	views, err := svc.ListTagsWithVerdicts(r.Context(), userID, id)
	if err != nil {
		writeIssueTrackerError(w, err)
		return
	}
	out := make([]issueTrackerTagDTO, 0, len(views))
	for _, v := range views {
		out = append(out, issueTrackerTagDTO{
			ID: cleanTrackerText(v.Tag.ID), Name: cleanTrackerText(v.Tag.Name),
			Owner:           issueTrackerAccountDTO{ID: cleanTrackerText(v.Tag.Owner.ID), Login: cleanTrackerText(v.Tag.Owner.Login)},
			Read:            toIssueTrackerSharingDTO(v.Tag.Read),
			Use:             toIssueTrackerSharingDTO(v.Tag.Use),
			AccountCanApply: v.AccountCanApply,
			OthersCanApply:  v.OthersCanApply,
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tags": out})
}

// LinkRepoToIssueTracker links a never-enabled repo to one issue tracker project and
// eligibility tag (PUT /api/repos/{id}/issue-tracker-link). Refusals are 409 with the
// reason; there is no unlink or relink.
func (h *Handler) LinkRepoToIssueTracker(w http.ResponseWriter, r *http.Request) {
	userID, svc, ok := h.issueTrackerRequest(w, r)
	if !ok {
		return
	}
	repoID, ok := httpx.PathUUID(w, r, "id", "repo")
	if !ok {
		return
	}
	var req linkRepoToIssueTrackerRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	connID, err := uuid.Parse(req.ConnectionID)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid connection_id")
		return
	}
	link, err := svc.LinkRepo(r.Context(), userID, repoID, connID, req.ProjectID, req.TagID)
	if err != nil {
		writeIssueTrackerError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"link": issueTrackerLinkDTO{
		RepoID: link.RepoID, ConnectionID: link.ConnectionID, ProjectID: link.ProjectID,
		ProjectShortName: link.ProjectShort, TagID: link.TagID, TagName: link.TagName, LinkedAt: link.LinkedAt,
	}})
}
