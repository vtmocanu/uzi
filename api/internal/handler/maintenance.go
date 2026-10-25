package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// maintenanceBodyLimit bounds the PUT body; the reason is sanitized to 280 characters after.
const maintenanceBodyLimit = 16 << 10

func timeStr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func pgTimeStr(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	return timeStr(&t.Time)
}

func pgTextStr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

func maintenanceItems(in []store.MaintenanceItem) []apitypes.MaintenanceItemDTO {
	out := make([]apitypes.MaintenanceItemDTO, 0, len(in))
	for _, it := range in {
		d := apitypes.MaintenanceItemDTO{RunID: it.RunID.String(), Kind: it.Kind, Status: it.Status,
			OwnerLabel: it.OwnerLabel, Repo: it.Repo, Since: timeStr(it.Since), Tag: it.Tag}
		if it.HoldID != nil {
			s := it.HoldID.String()
			d.HoldID = &s
		}
		out = append(out, d)
	}
	return out
}

func maintenanceSection(s store.MaintenanceSection) apitypes.MaintenanceSectionDTO {
	return apitypes.MaintenanceSectionDTO{Count: int(s.Count), Items: maintenanceItems(s.Items)}
}

func maintenanceReadoutDTO(m store.MaintenanceReadout) apitypes.MaintenanceReadoutDTO {
	workers := make([]apitypes.MaintenanceWorkerDTO, 0, len(m.IdleBoundWorkers.Items))
	for _, w := range m.IdleBoundWorkers.Items {
		workers = append(workers, apitypes.MaintenanceWorkerDTO{WorkerID: w.WorkerID.String(),
			OwnerLabel: w.OwnerLabel, RunID: w.RunID.String(), Since: timeStr(w.Since)})
	}
	return apitypes.MaintenanceReadoutDTO{
		AsOf:             m.AsOf.UTC().Format(time.RFC3339),
		Clear:            m.Clear(),
		Active:           maintenanceSection(m.Active),
		Held:             maintenanceSection(m.Held),
		Custody:          maintenanceSection(m.Custody),
		Queued:           maintenanceSection(m.Queued),
		IdleBoundWorkers: apitypes.MaintenanceWorkersDTO{Count: int(m.IdleBoundWorkers.Count), Items: workers},
	}
}

// GetMaintenance serves GET /api/maintenance to any authenticated user: only the public
// pause state, never attribution or the readout.
func (h *Handler) GetMaintenance(w http.ResponseWriter, r *http.Request) {
	st, err := store.ReadInstanceIntakePause(r.Context(), h.q)
	if err != nil {
		slog.Error("read intake pause", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.MaintenanceStateDTO{
		Active: st.Active, Since: pgTimeStr(st.Since), Reason: pgTextStr(st.Reason),
	})
}

// AdminGetMaintenance serves GET /api/admin/maintenance (RequireUser + RequireAdminRO).
func (h *Handler) AdminGetMaintenance(w http.ResponseWriter, r *http.Request) {
	h.writeMaintenanceAdmin(w, r)
}

// AdminPutMaintenance serves PUT /api/admin/maintenance (cookie-only RequireAuth +
// RequireAdmin, so an admin read token is refused and CSRF applies as for settings PUT).
// The actor comes from the session, never the body.
func (h *Handler) AdminPutMaintenance(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Active *bool   `json:"active"`
		Reason *string `json:"reason"`
	}
	if err := httpx.DecodeJSONStrictBounded(r, &req, maintenanceBodyLimit); err != nil || req.Active == nil {
		httpx.Error(w, http.StatusBadRequest, "invalid maintenance request")
		return
	}
	reason := ""
	if req.Reason != nil && *req.Active {
		reason = *req.Reason
	}
	label := user.DisplayName.String
	if label == "" {
		label = user.Email
	}
	if h.pool == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	ctx := r.Context()
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		slog.Error("maintenance begin", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, changed, err := store.SetInstanceIntakePause(ctx, store.New(tx), *req.Active, reason,
		pgtype.UUID{Bytes: user.ID, Valid: true}, label)
	if err != nil {
		slog.Error("maintenance set", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Commit on every nil error, a no-op included: the helper may have re-seeded a missing
	// singleton before concluding there was nothing more to change.
	if err := tx.Commit(ctx); err != nil {
		slog.Error("maintenance commit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if changed {
		slog.Info("instance intake pause changed", "active", row.Active, "by_user_id", user.ID)
	}
	h.writeMaintenanceAdmin(w, r)
}

func (h *Handler) writeMaintenanceAdmin(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	st, err := store.ReadInstanceIntakePause(r.Context(), h.q)
	if err != nil {
		slog.Error("read intake pause", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	readout, err := store.ReadMaintenanceReadout(r.Context(), h.pool)
	if err != nil {
		slog.Error("maintenance readout", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.MaintenanceAdminDTO{
		State: apitypes.MaintenanceAdminStateDTO{
			Active: st.Active, Since: pgTimeStr(st.Since), Reason: pgTextStr(st.Reason), ByLabel: pgTextStr(st.ByLabel),
		},
		Readout: maintenanceReadoutDTO(readout),
	})
}
