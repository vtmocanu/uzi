package handler

import (
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
)

// AdminRepoActivity is the admin cross-user repo-activity read (issue #4334). It is
// ADMIN-ONLY, mounted under the admin READ group (RequireUser + RequireAdminRO),
// backed by the UNSCOPED AdminListEnabledRepoActivity query (precedent
// AdminListBlockedRepos). It returns every repo that is enabled or has a listed-active
// run, with the time of its newest run. The count is exact regardless of the 500-run
// cap on the active-runs list, because the query has no LIMIT.
func (h *Handler) AdminRepoActivity(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.AdminListEnabledRepoActivity(r.Context())
	if err != nil {
		slog.Error("admin repo activity", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Non-nil so an empty result serializes repos as [] rather than null.
	out := make([]apitypes.RepoActivityDTO, 0, len(rows))
	for _, row := range rows {
		dto := apitypes.RepoActivityDTO{
			ID:                row.ID.String(),
			PathWithNamespace: row.PathWithNamespace,
			Enabled:           row.Enabled,
		}
		if row.LastRunAt.Valid {
			t := row.LastRunAt.Time.UTC()
			dto.LastRunAt = &t
		}
		out = append(out, dto)
	}

	httpx.JSON(w, http.StatusOK, apitypes.AdminRepoActivityDTO{Repos: out})
}
