package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var errSecretTransitionConflict = errors.New("choose an enabled replacement in Settings")

func secretRoute(r *http.Request) (uuid.UUID, bool) {
	kind := chi.URLParam(r, "kind")
	if kind != store.KindAnthropicToken && kind != "codex_auth" && kind != "openai_api_key" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	return id, err == nil
}

func (h *Handler) PatchSecretEnabled(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, 401, "authentication required")
		return
	}
	id, ok := secretRoute(r)
	if !ok {
		httpx.Error(w, 404, "secret not found")
		return
	}
	kind := chi.URLParam(r, "kind")
	var req struct {
		Enabled      *bool  `json:"enabled"`
		NewDefaultID string `json:"new_default_id"`
	}
	if httpx.DecodeJSON(r, &req) != nil || req.Enabled == nil {
		httpx.Error(w, 400, "enabled is required")
		return
	}
	var replacement uuid.UUID
	if req.NewDefaultID != "" {
		var err error
		replacement, err = uuid.Parse(req.NewDefaultID)
		if err != nil {
			httpx.Error(w, 400, "invalid new_default_id")
			return
		}
	}
	var row store.GetSecretEnablementRow
	found := false
	err := h.withSecretLock(r.Context(), user.ID, func(q *store.Queries) error {
		cur, err := q.GetUserSecretForUpdate(r.Context(), store.GetUserSecretForUpdateParams{ID: id, UserID: user.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if cur.Kind != kind {
			return nil
		}
		found = true
		if cur.DisabledAt.Valid == !*req.Enabled {
			row, err = q.GetSecretEnablement(r.Context(), store.GetSecretEnablementParams{ID: id, UserID: user.ID})
			return err
		}
		if !*req.Enabled && cur.IsDefault {
			count, err := q.CountEnabledSecretSlot(r.Context(), store.CountEnabledSecretSlotParams{UserID: user.ID, Kind: kind})
			if err != nil {
				return err
			}
			if count > 1 {
				if replacement == uuid.Nil || replacement == id {
					return errSecretTransitionConflict
				}
				_, err = q.GetEnabledSecretReplacement(r.Context(), store.GetEnabledSecretReplacementParams{ID: replacement, UserID: user.ID, Kind: kind})
				if errors.Is(err, pgx.ErrNoRows) {
					return errSecretTransitionConflict
				}
				if err != nil {
					return err
				}
			}
			if kind == store.KindAnthropicToken {
				_, err = q.ClearDefaultUserSecret(r.Context(), store.ClearDefaultUserSecretParams{UserID: user.ID, Kind: kind})
			} else {
				_, err = q.ClearCodexDefaults(r.Context(), user.ID)
			}
			if err != nil {
				return err
			}
			if count > 1 {
				_, err = q.SetUserSecretDefault(r.Context(), store.SetUserSecretDefaultParams{ID: replacement, UserID: user.ID})
				if err != nil {
					return err
				}
			}
		}
		_, err = q.SetSecretEnablement(r.Context(), store.SetSecretEnablementParams{ID: id, UserID: user.ID, Enabled: *req.Enabled})
		if err != nil {
			return err
		}
		if *req.Enabled {
			// Re-enable fills an empty default slot without displacing an existing default.
			if kind == store.KindAnthropicToken {
				current, e := q.GetDefaultUserSecretID(r.Context(), store.GetDefaultUserSecretIDParams{UserID: user.ID, Kind: kind})
				if e != nil && !errors.Is(e, pgx.ErrNoRows) {
					return e
				}
				if current == uuid.Nil {
					_, err = q.SetUserSecretDefault(r.Context(), store.SetUserSecretDefaultParams{ID: id, UserID: user.ID})
					if err != nil {
						return err
					}
				}
			} else {
				has, e := q.HasSecretDefaultSlot(r.Context(), store.HasSecretDefaultSlotParams{UserID: user.ID, Kind: kind})
				if e != nil {
					return e
				}
				if !has {
					_, err = q.SetUserSecretDefault(r.Context(), store.SetUserSecretDefaultParams{ID: id, UserID: user.ID})
					if err != nil {
						return err
					}
				}
			}
		}
		row, err = q.GetSecretEnablement(r.Context(), store.GetSecretEnablementParams{ID: id, UserID: user.ID})
		return err
	})
	if errors.Is(err, errSecretTransitionConflict) {
		httpx.Error(w, 409, err.Error())
		return
	}
	if err != nil {
		slog.Error("patch secret enablement", "error", err)
		httpx.Error(w, 500, "internal error")
		return
	}
	if !found {
		httpx.Error(w, 404, "secret not found")
		return
	}
	dto := secretMeta(row.ID, row.Kind, row.Label, row.IsDefault, row.AutoEligible, row.CreatedAt, row.UpdatedAt)
	dto.Enabled = !row.DisabledAt.Valid
	if row.DisabledAt.Valid {
		dto.DisabledAt = &row.DisabledAt.Time
	}
	httpx.JSON(w, 200, map[string]any{"secret": dto})
}

const secretDependentPageLimit = 50

type secretDependentPage[T any] struct {
	Items      []T    `json:"items"`
	Total      int64  `json:"total"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func dependentPage[T any](items []T, total int64, id func(T) uuid.UUID, limit int) secretDependentPage[T] {
	page := secretDependentPage[T]{Items: items, Total: total}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = id(page.Items[len(page.Items)-1]).String()
	}
	return page
}

func (h *Handler) GetSecretDependents(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, 401, "authentication required")
		return
	}
	id, ok := secretRoute(r)
	if !ok {
		httpx.Error(w, 404, "secret not found")
		return
	}
	row, err := h.q.GetSecretEnablement(r.Context(), store.GetSecretEnablementParams{ID: id, UserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.Kind != chi.URLParam(r, "kind")) {
		httpx.Error(w, 404, "secret not found")
		return
	}
	if err != nil {
		slog.Error("get secret dependents", "error", err)
		httpx.Error(w, 500, "internal error")
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, e := strconv.Atoi(raw)
		if e != nil || parsed < 1 || parsed > secretDependentPageLimit {
			httpx.Error(w, 400, "limit must be between 1 and 50")
			return
		}
		limit = parsed
	}
	cursors := make(map[string]uuid.UUID, 4)
	for _, name := range []string{"workers", "schedules", "runs", "enabled_siblings"} {
		if raw := r.URL.Query().Get(name + "_cursor"); raw != "" {
			cursor, e := uuid.Parse(raw)
			if e != nil {
				httpx.Error(w, 400, "invalid "+name+"_cursor")
				return
			}
			cursors[name] = cursor
		}
	}
	ctx := r.Context()
	deps, err := h.q.GetSecretDependents(ctx, store.GetSecretDependentsParams{UserID: user.ID, Column2: id})
	if err != nil {
		slog.Error("get secret dependents", "error", err)
		httpx.Error(w, 500, "internal error")
		return
	}
	secretID := pgtype.UUID{Bytes: id, Valid: true}
	workers, err := h.q.PageSecretDependentWorkers(ctx, store.PageSecretDependentWorkersParams{UserID: user.ID, SecretID: secretID, AfterID: cursors["workers"], PageSize: int32(limit + 1)})
	if err == nil {
		var schedules []store.PageSecretDependentSchedulesRow
		schedules, err = h.q.PageSecretDependentSchedules(ctx, store.PageSecretDependentSchedulesParams{UserID: user.ID, SecretID: secretID, AfterID: cursors["schedules"], PageSize: int32(limit + 1)})
		if err == nil {
			var runs []store.PageSecretDependentRunsRow
			runs, err = h.q.PageSecretDependentRuns(ctx, store.PageSecretDependentRunsParams{UserID: user.ID, SecretID: secretID, AfterID: cursors["runs"], PageSize: int32(limit + 1)})
			if err == nil {
				var siblings []store.PageSecretDependentSiblingsRow
				siblings, err = h.q.PageSecretDependentSiblings(ctx, store.PageSecretDependentSiblingsParams{UserID: user.ID, SecretID: id, AfterID: cursors["enabled_siblings"], PageSize: int32(limit + 1)})
				if err == nil {
					httpx.JSON(w, 200, map[string]any{
						"default": row.IsDefault, "judge": deps.Judge > 0,
						"workers":          dependentPage(workers, deps.Workers, func(item store.PageSecretDependentWorkersRow) uuid.UUID { return item.ID }, limit),
						"schedules":        dependentPage(schedules, deps.Schedules, func(item store.PageSecretDependentSchedulesRow) uuid.UUID { return item.ID }, limit),
						"runs":             dependentPage(runs, deps.Runs, func(item store.PageSecretDependentRunsRow) uuid.UUID { return item.ID }, limit),
						"enabled_siblings": dependentPage(siblings, deps.EnabledSiblings, func(item store.PageSecretDependentSiblingsRow) uuid.UUID { return item.ID }, limit),
					})
					return
				}
			}
		}
	}
	slog.Error("get secret dependent page", "error", err)
	httpx.Error(w, 500, "internal error")
}
