package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
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

// runOutcomes assembles a RunOutcomesDTO from the counts and a (possibly nil) origins map
// (PRD #1293). needsLanding is the server-computed sub-cut of failed (issue #1418), always
// needs_landing <= failed. It guarantees a non-nil fail_origins so the field marshals as
// {} rather than null, per the API contract — a nil map JSON-encodes to null.
func runOutcomes(finished, completed, cancelled, planRejected, failed, needsLanding int64, origins map[string]int64) apitypes.RunOutcomesDTO {
	if origins == nil {
		origins = map[string]int64{}
	}
	return apitypes.RunOutcomesDTO{
		Finished:     finished,
		Completed:    completed,
		Cancelled:    cancelled,
		PlanRejected: planRejected,
		Failed:       failed,
		NeedsLanding: needsLanding,
		FailOrigins:  origins,
	}
}

// decodeFailOrigins decodes a jsonb fail_origins column (issue #1451: the per-origin
// breakdown computed in the same statement as the `failed` count) into the DTO map. The
// query COALESCEs an empty group to '{}'; an absent column (nil, e.g. a zero-value row) also
// yields an empty non-nil map. Malformed JSON is an error the caller turns into a 500.
func decodeFailOrigins(raw []byte) (map[string]int64, error) {
	origins := map[string]int64{}
	if len(raw) == 0 {
		return origins, nil
	}
	if err := json.Unmarshal(raw, &origins); err != nil {
		return nil, fmt.Errorf("decode fail_origins: %w", err)
	}
	return origins, nil
}

// SelfUsage returns the requesting user's own usage (PRD #40): lifetime and
// last-7-days totals, usage-bearing run count, failed-run outcomes (PRD #1293),
// and subscription/unreported counts. RequireUser accepts a session or CLI Bearer;
// every self query uses the authenticated user's context ID.
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
	outcomes, err := h.wsvc.SelfRunOutcomes(r.Context(), user.ID)
	if err != nil {
		slog.Error("self run outcomes", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// The per-origin breakdown rides the same row as the counts (issue #1451), so
	// sum(fail_origins) == failed holds within this response by construction.
	lifetimeOrigins, err := decodeFailOrigins(outcomes.LifetimeFailOrigins)
	if err != nil {
		slog.Error("self run outcomes", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	last7Origins, err := decodeFailOrigins(outcomes.Last7FailOrigins)
	if err != nil {
		slog.Error("self run outcomes", "error", err)
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
		Outcomes: apitypes.RunOutcomeWindowsDTO{
			Lifetime: runOutcomes(outcomes.LifetimeFinished, outcomes.LifetimeCompleted,
				outcomes.LifetimeCancelled, outcomes.LifetimePlanRejected, outcomes.LifetimeFailed,
				outcomes.LifetimeNeedsLanding, lifetimeOrigins),
			Last7Days: runOutcomes(outcomes.Last7Finished, outcomes.Last7Completed,
				outcomes.Last7Cancelled, outcomes.Last7PlanRejected, outcomes.Last7Failed,
				outcomes.Last7NeedsLanding, last7Origins),
		},
		// PRD #1429 M1 (D7): the per-window subscription/unreported run counts the store
		// already computes, so the summary can disclose that a non-metered component makes the
		// numeric dollar total incomplete rather than presenting a partial sum as complete.
		LifetimeSubscriptionRunCount: row.LifetimeSubscriptionRunCount,
		LifetimeUnreportedRunCount:   row.LifetimeUnreportedRunCount,
		Last7SubscriptionRunCount:    row.Last7SubscriptionRunCount,
		Last7UnreportedRunCount:      row.Last7UnreportedRunCount,
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
	// PRD #1293: the failed-run rate outcome aggregates, a separate scan over `runs` (D1)
	// merged into the usage rows by user id (D5).
	factoryOutcomes, err := h.wsvc.AdminRunOutcomes(r.Context())
	if err != nil {
		slog.Error("admin run outcomes", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// The per-origin breakdowns ride the same rows as the counts (issue #1451), so
	// sum(fail_origins) == failed holds per scope within this response by construction.
	factoryLifetimeOrigins, err := decodeFailOrigins(factoryOutcomes.LifetimeFailOrigins)
	if err != nil {
		slog.Error("admin run outcomes", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	factoryLast7Origins, err := decodeFailOrigins(factoryOutcomes.Last7FailOrigins)
	if err != nil {
		slog.Error("admin run outcomes", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	perUserOutcomes, err := h.wsvc.AdminRunOutcomesPerUser(r.Context())
	if err != nil {
		slog.Error("admin run outcomes per user", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Index the per-user outcome counts (and their decoded lifetime origins) by user id, so
	// a usage row can attach its outcomes.
	outcomesByUser := map[uuid.UUID]store.AdminRunOutcomesPerUserRow{}
	originsByUser := map[uuid.UUID]map[string]int64{}
	for _, o := range perUserOutcomes {
		origins, err := decodeFailOrigins(o.FailOrigins)
		if err != nil {
			slog.Error("admin run outcomes per user", "error", err, "user_id", o.UserID)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		outcomesByUser[o.UserID] = o
		originsByUser[o.UserID] = origins
	}

	users := make([]apitypes.AdminUserUsageDTO, 0, len(rows)+len(perUserOutcomes))
	seen := map[uuid.UUID]bool{}
	for _, u := range rows {
		seen[u.UserID] = true
		// A user with no outcomes row gets a zero-value RunOutcomesDTO (all-zero counts).
		oc := outcomesByUser[u.UserID]
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
			// PRD #1429 M1 (D7): the user's lifetime subscription/unreported run counts, so the
			// admin per-user breakdown discloses a non-metered component like the factory total.
			SubscriptionRunCount: u.SubscriptionRunCount,
			UnreportedRunCount:   u.UnreportedRunCount,
			Outcomes: runOutcomes(oc.Finished, oc.Completed, oc.Cancelled, oc.PlanRejected, oc.Failed,
				oc.NeedsLanding, originsByUser[u.UserID]),
		})
	}
	// D5: a user present in the outcomes aggregate but ABSENT from usage (every run died
	// before spending, so no usage row) gets a zero-usage row APPENDED after the
	// cost-sorted usage rows, so the heaviest-first order of the usage rows is untouched.
	for _, oc := range perUserOutcomes {
		if seen[oc.UserID] {
			continue
		}
		users = append(users, apitypes.AdminUserUsageDTO{
			UserID:   oc.UserID.String(),
			Email:    oc.Email,
			Usage:    apitypes.UsageDTO{},
			RunCount: 0,
			// No usage row means no run_usage_totals rows, so both non-metered counts are zero.
			SubscriptionRunCount: 0,
			UnreportedRunCount:   0,
			Outcomes: runOutcomes(oc.Finished, oc.Completed, oc.Cancelled, oc.PlanRejected, oc.Failed,
				oc.NeedsLanding, originsByUser[oc.UserID]),
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
			Outcomes: apitypes.RunOutcomeWindowsDTO{
				Lifetime: runOutcomes(factoryOutcomes.LifetimeFinished, factoryOutcomes.LifetimeCompleted,
					factoryOutcomes.LifetimeCancelled, factoryOutcomes.LifetimePlanRejected,
					factoryOutcomes.LifetimeFailed, factoryOutcomes.LifetimeNeedsLanding, factoryLifetimeOrigins),
				Last7Days: runOutcomes(factoryOutcomes.Last7Finished, factoryOutcomes.Last7Completed,
					factoryOutcomes.Last7Cancelled, factoryOutcomes.Last7PlanRejected,
					factoryOutcomes.Last7Failed, factoryOutcomes.Last7NeedsLanding, factoryLast7Origins),
			},
			// PRD #1429 M1 (D7): the factory-wide per-window subscription/unreported run
			// counts, so a mixed aggregate never presents its numeric metered subset as the
			// complete total.
			LifetimeSubscriptionRunCount: totals.LifetimeSubscriptionRunCount,
			LifetimeUnreportedRunCount:   totals.LifetimeUnreportedRunCount,
			Last7SubscriptionRunCount:    totals.Last7SubscriptionRunCount,
			Last7UnreportedRunCount:      totals.Last7UnreportedRunCount,
		},
		Users:       users,
		EarliestRun: timePtr(totals.EarliestRun.Valid, totals.EarliestRun.Time),
	})
}

// usageFromTotals builds the run-level usage bundle for a run list row from its
// ListRunUsageTotalsForRuns row (issue #1620), or nil when the run has no usage (ok is
// false: the run is absent from the page's totals map — a pre-feature run shows nothing,
// never a fake 0).
func usageFromTotals(row store.RunUsageTotal, ok bool) *apitypes.UsageDTO {
	if !ok {
		return nil
	}
	return &apitypes.UsageDTO{
		InputTokens:         row.InputTokens,
		CacheReadTokens:     row.CacheReadTokens,
		CacheCreationTokens: row.CacheCreationTokens,
		OutputTokens:        row.OutputTokens,
		CostUSD:             numericToFloat(row.CostUsd),
		// PRD #1429 M1 (D7): the run's folded cost_status from run_usage_totals, so a
		// subscription/unreported placeholder 0 never reads as a real dollar total.
		CostStatus: row.CostStatus,
	}
}
