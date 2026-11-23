package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// runWritersMaxRows is the most writers GET /api/admin/run-writers serves. The query asks
// for one more so a full page can be told from a complete result.
const runWritersMaxRows = 500

// listRunWriters is the store read behind AdminRunWriters, a variable so a unit test can
// exercise the handler without a database.
var listRunWriters = func(ctx context.Context, pool *pgxpool.Pool, repoID uuid.UUID, branch string, mrIID *int64, maxRows int32) ([]store.ListRunWritersForTargetRow, error) {
	return store.ListRunWriters(ctx, pool, repoID, branch, mrIID, maxRows)
}

// runWritersSingleParam returns the single value of key, or false when the key is
// repeated (ambiguous, so refused).
func runWritersSingleParam(q url.Values, key string) (string, bool) {
	vals := q[key]
	if len(vals) > 1 {
		return "", false
	}
	if len(vals) == 0 {
		return "", true
	}
	return vals[0], true
}

// parseCanonicalPositiveInt64 accepts only the canonical decimal spelling of a positive
// int64: no sign, no leading zeros, no whitespace.
func parseCanonicalPositiveInt64(s string) (int64, bool) {
	n, err := parseInt64(s)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != s {
		return 0, false
	}
	return n, true
}

// AdminRunWriters serves GET /api/admin/run-writers?repo=<uuid>&branch=<ref>[&mr_iid=<n>]
// (issue #4618): the active ci_fix / mr_rework runs, of every owner, on the repo's forge
// project: ci_fix runs matched by branch, mr_rework runs matched by MR (mr_iid) only. The
// admin-debug predicate is checked before any parameter is read, so a caller who may not
// debug learns nothing about which parameters were acceptable.
func (h *Handler) AdminRunWriters(w http.ResponseWriter, r *http.Request) {
	actor, tokenID, ok := h.adminRunDebugActor(r)
	if !ok {
		httpx.Error(w, http.StatusForbidden, adminRunDebugRefusal)
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "malformed query string")
		return
	}
	for key := range q {
		if key != "repo" && key != "branch" && key != "mr_iid" {
			httpx.Error(w, http.StatusBadRequest, "unknown query parameter; use repo, branch and mr_iid")
			return
		}
	}
	repoRaw, ok1 := runWritersSingleParam(q, "repo")
	branch, ok2 := runWritersSingleParam(q, "branch")
	mrRaw, ok3 := runWritersSingleParam(q, "mr_iid")
	if !ok1 || !ok2 || !ok3 {
		httpx.Error(w, http.StatusBadRequest, "repo, branch and mr_iid may each be given once")
		return
	}
	repoID, err := uuid.Parse(repoRaw)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid repo id")
		return
	}
	if !workersvc.ValidBranchName(branch) {
		httpx.Error(w, http.StatusBadRequest, "invalid branch")
		return
	}
	var mrIID *int64
	if q.Has("mr_iid") {
		n, valid := parseCanonicalPositiveInt64(mrRaw)
		if !valid {
			httpx.Error(w, http.StatusBadRequest, "invalid mr_iid; use a positive decimal integer")
			return
		}
		mrIID = &n
	}
	rows, err := listRunWriters(r.Context(), h.pool, repoID, branch, mrIID, runWritersMaxRows+1)
	if err != nil {
		if errors.Is(err, store.ErrRunWritersRepoNotFound) {
			httpx.Error(w, http.StatusUnprocessableEntity, "repo not found")
			return
		}
		slog.Error("admin run writers", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	complete := len(rows) <= runWritersMaxRows
	if !complete {
		rows = rows[:runWritersMaxRows]
	}
	writers := make([]apitypes.RunWriterDTO, 0, len(rows))
	for _, row := range rows {
		dto := apitypes.RunWriterDTO{
			RunID:   row.ID.String(),
			Kind:    row.Kind,
			Status:  row.Status,
			OwnerID: row.UserID.String(),
		}
		if row.RepoID.Valid {
			dto.RepoID = uuid.UUID(row.RepoID.Bytes).String()
		}
		if row.PipelineRef.Valid {
			ref := row.PipelineRef.String
			dto.PipelineRef = &ref
		}
		if row.MrIid.Valid {
			iid := row.MrIid.Int64
			dto.MRIID = &iid
		}
		dto.CreatedAt = row.CreatedAt.Time
		writers = append(writers, dto)
	}
	auditRunWritersDebugRead(r, actor, tokenID, adminRunDebugRouteWriters, repoID, mrIID, len(branch), len(writers), complete)
	httpx.JSON(w, http.StatusOK, apitypes.RunWritersDTO{Writers: writers, Complete: complete})
}
