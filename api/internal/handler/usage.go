package handler

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// usageDTO/selfUsageDTO/adminUserUsageDTO/adminUsageDTO moved to the stdlib-only
// apitypes leaf (PRD #64 M1); the handlers below build them from store rows.

// numericToFloat renders a pgtype.Numeric (a summed cost_usd) as a JSON number.
// An invalid/unset numeric — and any conversion failure — folds to 0 rather than
// surfacing a NaN; costs are always finite and non-negative here.
func numericToFloat(n pgtype.Numeric) float64 {
	if !n.Valid {
		return 0
	}
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return 0
	}
	return f.Float64
}

// SelfUsage returns the requesting user's own usage (PRD #40): lifetime totals,
// last-7-days totals, and their usage-bearing run count. Session-authed; scoped to
// the caller, so a user only ever sees their own consumption.
func (h *Handler) SelfUsage(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	row, err := h.wsvc.SelfUsage(r.Context(), user.ID)
	if err != nil {
		slog.Error("self usage", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.SelfUsageDTO{
		Lifetime: apitypes.UsageDTO{
			InputTokens:         row.LifetimeInputTokens,
			CacheReadTokens:     row.LifetimeCacheReadTokens,
			CacheCreationTokens: row.LifetimeCacheCreationTokens,
			OutputTokens:        row.LifetimeOutputTokens,
			CostUSD:             numericToFloat(row.LifetimeCostUsd),
		},
		Last7Days: apitypes.UsageDTO{
			InputTokens:         row.Last7InputTokens,
			CacheReadTokens:     row.Last7CacheReadTokens,
			CacheCreationTokens: row.Last7CacheCreationTokens,
			OutputTokens:        row.Last7OutputTokens,
			CostUSD:             numericToFloat(row.Last7CostUsd),
		},
		RunCount: row.RunCount,
	})
}

// AdminUsage returns factory-wide totals plus the per-user breakdown (PRD #40).
// Admin-only — the route is under the RequireAdmin group, so a non-admin never
// reaches here and never sees another user's consumption.
func (h *Handler) AdminUsage(w http.ResponseWriter, r *http.Request) {
	totals, err := h.wsvc.AdminUsageTotals(r.Context())
	if err != nil {
		slog.Error("admin usage totals", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, err := h.wsvc.AdminUsagePerUser(r.Context())
	if err != nil {
		slog.Error("admin usage per user", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	users := make([]apitypes.AdminUserUsageDTO, 0, len(rows))
	for _, u := range rows {
		users = append(users, apitypes.AdminUserUsageDTO{
			UserID: u.UserID.String(),
			Email:  u.Email,
			Usage: apitypes.UsageDTO{
				InputTokens:         u.InputTokens,
				CacheReadTokens:     u.CacheReadTokens,
				CacheCreationTokens: u.CacheCreationTokens,
				OutputTokens:        u.OutputTokens,
				CostUSD:             numericToFloat(u.CostUsd),
			},
			RunCount: u.RunCount,
		})
	}
	httpx.JSON(w, http.StatusOK, apitypes.AdminUsageDTO{
		Factory: apitypes.SelfUsageDTO{
			Lifetime: apitypes.UsageDTO{
				InputTokens:         totals.LifetimeInputTokens,
				CacheReadTokens:     totals.LifetimeCacheReadTokens,
				CacheCreationTokens: totals.LifetimeCacheCreationTokens,
				OutputTokens:        totals.LifetimeOutputTokens,
				CostUSD:             numericToFloat(totals.LifetimeCostUsd),
			},
			Last7Days: apitypes.UsageDTO{
				InputTokens:         totals.Last7InputTokens,
				CacheReadTokens:     totals.Last7CacheReadTokens,
				CacheCreationTokens: totals.Last7CacheCreationTokens,
				OutputTokens:        totals.Last7OutputTokens,
				CostUSD:             numericToFloat(totals.Last7CostUsd),
			},
			RunCount: totals.RunCount,
		},
		Users:       users,
		EarliestRun: timePtr(totals.EarliestRun.Valid, totals.EarliestRun.Time),
	})
}

// usageFromListRow builds the run-level usage bundle for a run list row, or nil
// when the run has no usage (the LEFT JOIN yields NULLs — a pre-feature run shows
// nothing, never a fake 0). All usage_* columns are NULL together, so the input
// token column's validity gates the whole bundle.
func usageFromListRow(row store.ListRunsForUserRow) *apitypes.UsageDTO {
	if !row.UsageInputTokens.Valid {
		return nil
	}
	return &apitypes.UsageDTO{
		InputTokens:         row.UsageInputTokens.Int64,
		CacheReadTokens:     row.UsageCacheReadTokens.Int64,
		CacheCreationTokens: row.UsageCacheCreationTokens.Int64,
		OutputTokens:        row.UsageOutputTokens.Int64,
		CostUSD:             numericToFloat(row.UsageCostUsd),
	}
}
