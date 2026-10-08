package handler

import (
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
)

// AdminListDockerAllowlistRepos lists enabled repositories and disabled repositories
// retained by the effective Docker allowlist. Authorization is on the admin read group.
func (h *Handler) AdminListDockerAllowlistRepos(w http.ResponseWriter, r *http.Request) {
	allowlist, err := h.settings.DockerRepoAllowlist(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, err := h.q.AdminListDockerAllowlistRepos(r.Context(), allowlist)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := apitypes.AdminDockerAllowlistReposDTO{Repos: make([]apitypes.AdminDockerAllowlistRepoDTO, 0, len(rows))}
	for _, row := range rows {
		out.Repos = append(out.Repos, apitypes.AdminDockerAllowlistRepoDTO{
			ID: row.ID.String(), PathWithNamespace: row.PathWithNamespace, Enabled: row.Enabled,
			OwnerEmail: row.OwnerEmail, ConnectionID: row.ConnectionID.String(),
			ForgeType: row.ForgeType, BaseURL: row.BaseUrl,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}
