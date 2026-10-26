package handler

import (
	"context"
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

// maintenanceLabelMax caps a rendered owner label or repo path.
const maintenanceLabelMax = 200

// termSafe sanitizes user-authored text (display name, email, repo path) at the render
// boundary: ESC and other control sequences, bidi overrides and newlines never reach the
// admin readout's consumers (terminal CLI, TUI).
func termSafe(s string) string { return store.ManualCordonText(s, maintenanceLabelMax) }

func termSafePtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := termSafe(*s)
	return &v
}

func maintenanceItems(in []store.MaintenanceItem) []apitypes.MaintenanceItemDTO {
	out := make([]apitypes.MaintenanceItemDTO, 0, len(in))
	for _, it := range in {
		d := apitypes.MaintenanceItemDTO{RunID: it.RunID.String(), Kind: it.Kind, Status: it.Status,
			OwnerLabel: termSafe(it.OwnerLabel), Repo: termSafePtr(it.Repo), Since: timeStr(it.Since), Tag: it.Tag}
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
			OwnerLabel: termSafe(w.OwnerLabel), RunID: w.RunID.String(), Since: timeStr(w.Since)})
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

// maintenanceNotice returns the notice for a run just created, or "" when maintenance is off.
// It reads the pause row uncached after the create committed. A read error is logged and
// yields no notice: the run was created and the notice is advisory.
func (h *Handler) maintenanceNotice(ctx context.Context) string {
	if h.q == nil { // handlers wired without a store (unit tests) have no pause to report
		return ""
	}
	st, err := store.ReadInstanceIntakePause(ctx, h.q)
	if err != nil {
		slog.Warn("read intake pause for create notice", "error", err)
		return ""
	}
	return store.MaintenanceNoticeText(st)
}

// createdRunBody is the {"run": dto} create response, plus "notice" while maintenance is on.
func (h *Handler) createdRunBody(ctx context.Context, run any) map[string]any {
	body := map[string]any{"run": run}
	if n := h.maintenanceNotice(ctx); n != "" {
		body["notice"] = n
	}
	return body
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
	// Commit on every nil error: a change must persist, and even a no-op may have re-seeded
	// a missing singleton (a true no-op writes nothing, so committing it is harmless).
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
	st, readout, err := store.ReadMaintenance(r.Context(), h.pool)
	if err != nil {
		slog.Error("maintenance readout", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.MaintenanceAdminDTO{State: maintenanceStateDTO(st), Readout: maintenanceReadoutDTO(readout)})
}

// maintenanceStateDTO maps the pause row to its wire form. by_label is the user-authored label of
// whoever flipped the pause, so it is sanitized at this boundary like the readout labels.
func maintenanceStateDTO(st store.IntakePauseState) apitypes.MaintenanceAdminStateDTO {
	return apitypes.MaintenanceAdminStateDTO{
		Active: st.Active, Since: pgTimeStr(st.Since), Reason: pgTextStr(st.Reason), ByLabel: termSafePtr(pgTextStr(st.ByLabel)),
	}
}
