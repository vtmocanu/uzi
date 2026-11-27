package handler

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4731: GET /api/analytics/capacity and GET /api/admin/analytics/capacity.
//
// One REPEATABLE READ snapshot holds, in order: the shared authority checks, the history-start
// read, the bucket guard, the historical occupancy and usage-wait statement, then each selected
// provider's current gauge statement inside its own savepoint (Anthropic, then Codex). A gauge
// that fails with an ordinary error is marked unavailable and the rest of the response is kept;
// a statement timeout or the snapshot deadline fails the whole request like any analytics
// timeout. Codex vault state is not part of the snapshot: it is observed live afterwards, and
// on the admin routes only without waiting for any user's lifecycle lock (vault.TryStatus).
// Nothing here calls a provider.

const (
	// capacityIdentityCap is the number of identities returned per provider gauge; the gauge
	// statements fetch one more to detect truncation.
	capacityIdentityCap = 200
	// capacityVaultObserveBudget is the admin route's serial vault observation budget. It is
	// checked between and after the lookups; reads run under the budget's context (a step that
	// ignores cancellation can still finish after it), and TryStatus never waits for a lifecycle lock.
	capacityVaultObserveBudget = 2 * time.Second

	capacityTimeBasisOccupancy   = "interval_overlap"
	capacityUnitOccupancy        = "average_concurrent_runs"
	capacityTimeBasisUsageWait   = "created_at"
	capacityCohortUsageWait      = "creation"
	capacityBasisUsageWait       = "limit_wait_count"
	capacityTimeBasisGauge       = "current_observation"
	capacityVaultStateBasis      = "live_observation"
	capacityGaugeAvailable       = "available"
	capacityGaugeUnavailable     = "unavailable"
	capacityGaugeNotSelected     = "not_selected"
	capacitySetEmpty             = "empty"
	capacitySetComplete          = "complete"
	capacitySetTruncated         = "truncated"
	capacityProviderAnthropic    = "anthropic"
	capacityProviderCodex        = "codex"
	capacitySelectorRepo         = "repo"
	capacitySelectorKind         = "kind"
	capacityExcludedKindSelected = runkind.Chat
)

// capacityCodexRow is one Codex gauge row from either statement shape. UserID, Email and
// VaultMode are set only on the all-users shape.
type capacityCodexRow struct {
	userID         uuid.UUID
	email          string
	vaultMode      string
	accountID      uuid.UUID
	aliases        []string
	isDefault      bool
	reauthRequired bool
	reauthReason   pgtype.Text
	buckets        []byte
	lastSuccessAt  pgtype.Timestamptz
	lastAttemptAt  pgtype.Timestamptz
}

// capacityResult is what the snapshot yields: the finished response except the Codex gauge,
// whose statuses need the vault observation.
type capacityResult struct {
	dto   apitypes.AnalyticsCapacityDTO
	codex capacityCodexSet
}

// capacityCodexSet is the Codex gauge as read inside the snapshot.
type capacityCodexSet struct {
	selected  bool
	failed    bool // the gauge read failed with an ordinary error
	rows      []capacityCodexRow
	truncated bool
}

// providerSelected reports whether the harness selector includes the provider.
func (r analyticsRequest) providerSelected(provider string) bool {
	switch r.harness {
	case "":
		return true
	case "claude":
		return provider == capacityProviderAnthropic
	case "codex":
		return provider == capacityProviderCodex
	}
	return false
}

// capacity runs the Capacity flow inside the snapshot.
func (h *Handler) capacity(ctx context.Context, d *analyticsDeps, s *store.AnalyticsSnapshot, req analyticsRequest, now time.Time) (*capacityResult, error) {
	loc, err := d.authorize(ctx, s, req)
	if err != nil {
		return nil, err
	}
	earliest, err := d.historyStart(ctx, s, req)
	if err != nil {
		return nil, err
	}
	bounds, err := analytics.ComputeBounds(now, loc, req.window, earliest)
	if err != nil {
		return nil, err
	}
	if err := analytics.ValidateBuckets(bounds); err != nil {
		return nil, err
	}

	var repo pgtype.UUID
	if req.repo != uuid.Nil {
		repo = pgtype.UUID{Bytes: req.repo, Valid: true}
	}
	var harness pgtype.Text
	if req.harness != "" {
		harness = pgtype.Text{String: req.harness, Valid: true}
	}
	var raw []byte
	if req.allUsers {
		raw, err = s.Q.AnalyticsAllCapacity(ctx, store.AnalyticsAllCapacityParams{
			Now: analyticsTS(bounds.Now), From: analyticsTS(bounds.From), RepoID: repo, Harness: harness, Kinds: req.kinds,
			BucketStarts: analyticsTSList(bounds.BucketStarts), BucketEnds: analyticsTSList(bounds.BucketEnds),
		})
	} else {
		raw, err = s.Q.AnalyticsOwnerCapacity(ctx, store.AnalyticsOwnerCapacityParams{
			Now: analyticsTS(bounds.Now), From: analyticsTS(bounds.From), UserID: req.scopeUser, RepoID: repo, Harness: harness, Kinds: req.kinds,
			BucketStarts: analyticsTSList(bounds.BucketStarts), BucketEnds: analyticsTSList(bounds.BucketEnds),
		})
	}
	if err != nil {
		return nil, err
	}
	doc, err := store.DecodeAnalyticsCapacityDoc(raw)
	if err != nil {
		return nil, err
	}
	dto := mapAnalyticsCapacity(req, bounds, doc)

	res := &capacityResult{dto: dto}
	if res.dto.Gauges.Anthropic, err = h.anthropicGauge(ctx, d, s, req, now); err != nil {
		return nil, err
	}
	if res.codex, err = h.readCodexGauge(ctx, d, s, req); err != nil {
		return nil, err
	}
	return res, nil
}

// capacityEffectiveScope names the owner and provider a gauge describes and the selectors the
// request carried that cannot partition its shared quota.
func capacityEffectiveScope(req analyticsRequest, provider string) apitypes.AnalyticsCapacityEffectiveScopeDTO {
	sel := []string{}
	if req.repo != uuid.Nil {
		sel = append(sel, capacitySelectorRepo)
	}
	if req.kindName != "" {
		sel = append(sel, capacitySelectorKind)
	}
	out := apitypes.AnalyticsCapacityEffectiveScopeDTO{User: req.scope().User, Provider: provider, UnattributedSelectors: sel}
	if len(sel) > 0 {
		reason := store.AnalyticsReasonSharedQuotaNotAttributable
		out.Reason = &reason
	}
	return out
}

func capacityGaugeBase(req analyticsRequest, provider, status string, reason *string) apitypes.AnalyticsCapacityGaugeBaseDTO {
	return apitypes.AnalyticsCapacityGaugeBaseDTO{
		Provider:       provider,
		Status:         status,
		Reason:         reason,
		TimeBasis:      capacityTimeBasisGauge,
		EffectiveScope: capacityEffectiveScope(req, provider),
	}
}

func capacityReason(r string) *string { return &r }

// capacitySetState is the completeness of an available gauge's identity set.
func capacitySetState(n int, truncated bool) *string {
	switch {
	case truncated:
		return capacityReason(capacitySetTruncated)
	case n == 0:
		return capacityReason(capacitySetEmpty)
	}
	return capacityReason(capacitySetComplete)
}

// anthropicGauge reads the Claude gauge in its own savepoint. Only the savepoint's fatal error
// is returned; an ordinary read error marks the gauge unavailable.
func (h *Handler) anthropicGauge(ctx context.Context, d *analyticsDeps, s *store.AnalyticsSnapshot, req analyticsRequest, now time.Time) (apitypes.AnalyticsCapacityAnthropicGaugeDTO, error) {
	out := apitypes.AnalyticsCapacityAnthropicGaugeDTO{Identities: []apitypes.AnalyticsCapacityAnthropicIdentityDTO{}}
	if !req.providerSelected(capacityProviderAnthropic) {
		out.AnalyticsCapacityGaugeBaseDTO = capacityGaugeBase(req, capacityProviderAnthropic, capacityGaugeNotSelected, capacityReason(store.AnalyticsReasonNotSelected))
		return out, nil
	}
	var ids []apitypes.AnalyticsCapacityAnthropicIdentityDTO
	var truncated bool
	gaugeErr, err := s.Savepoint(ctx, func(gctx context.Context, q *store.Queries, db store.DBTX) error {
		if d.beforeGauge != nil {
			if err := d.beforeGauge(gctx, db, capacityProviderAnthropic); err != nil {
				return err
			}
		}
		if req.allUsers {
			rows, err := q.AnalyticsAllAnthropicGauges(gctx)
			if err != nil {
				return err
			}
			rows, truncated = capGauge(rows)
			ids = make([]apitypes.AnalyticsCapacityAnthropicIdentityDTO, 0, len(rows))
			for _, r := range rows {
				uid, email := r.UserID.String(), r.Email
				ids = append(ids, h.anthropicIdentity(r.UserSecretID, r.Label, r.IsDefault, &uid, &email,
					r.FiveHourPct, r.FiveHourResetsAt, r.SevenDayPct, r.SevenDayResetsAt, r.Source, r.SyncedAt, now))
			}
			return nil
		}
		rows, err := q.AnalyticsOwnerAnthropicGauges(gctx, req.scopeUser)
		if err != nil {
			return err
		}
		rows, truncated = capGauge(rows)
		ids = make([]apitypes.AnalyticsCapacityAnthropicIdentityDTO, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, h.anthropicIdentity(r.UserSecretID, r.Label, r.IsDefault, nil, nil,
				r.FiveHourPct, r.FiveHourResetsAt, r.SevenDayPct, r.SevenDayResetsAt, r.Source, r.SyncedAt, now))
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	if gaugeErr != nil {
		slog.Warn("analytics capacity: anthropic gauge unavailable", "error", gaugeErr)
		out.AnalyticsCapacityGaugeBaseDTO = capacityGaugeBase(req, capacityProviderAnthropic, capacityGaugeUnavailable, capacityReason(store.AnalyticsReasonGaugeUnavailable))
		return out, nil
	}
	out.AnalyticsCapacityGaugeBaseDTO = capacityGaugeBase(req, capacityProviderAnthropic, capacityGaugeAvailable, nil)
	out.Truncated = truncated
	out.SetState = capacitySetState(len(ids), truncated)
	out.Identities = ids
	return out, nil
}

// capGauge keeps the first capacityIdentityCap rows and reports whether more were fetched.
func capGauge[T any](rows []T) ([]T, bool) {
	if len(rows) > capacityIdentityCap {
		return rows[:capacityIdentityCap], true
	}
	return rows, false
}

// anthropicIdentity maps one token row. A token with no current-revision reading (the LEFT JOIN
// missed) is unavailable, as on GET /api/me/rate-limits.
func (h *Handler) anthropicIdentity(secretID uuid.UUID, label string, isDefault bool, userID, email *string,
	fivePct pgtype.Int2, fiveReset pgtype.Timestamptz, sevenPct pgtype.Int2, sevenReset pgtype.Timestamptz,
	source pgtype.Text, syncedAt pgtype.Timestamptz, now time.Time) apitypes.AnalyticsCapacityAnthropicIdentityDTO {
	limits := apitypes.RateLimitDTO{Status: rateLimitStatusUnavailable}
	if syncedAt.Valid {
		limits = h.okRateLimitDTO(fivePct, fiveReset, sevenPct, sevenReset, source, syncedAt, now)
	}
	return apitypes.AnalyticsCapacityAnthropicIdentityDTO{
		SecretID: secretID.String(), Label: label, IsDefault: isDefault, UserID: userID, Email: email, Limits: limits,
	}
}

// readCodexGauge reads the Codex gauge rows in their own savepoint. The statuses, which depend
// on the vault observation, are derived afterwards.
func (h *Handler) readCodexGauge(ctx context.Context, d *analyticsDeps, s *store.AnalyticsSnapshot, req analyticsRequest) (capacityCodexSet, error) {
	set := capacityCodexSet{selected: req.providerSelected(capacityProviderCodex)}
	if !set.selected {
		return set, nil
	}
	var rows []capacityCodexRow
	var truncated bool
	gaugeErr, err := s.Savepoint(ctx, func(gctx context.Context, q *store.Queries, db store.DBTX) error {
		if d.beforeGauge != nil {
			if err := d.beforeGauge(gctx, db, capacityProviderCodex); err != nil {
				return err
			}
		}
		if req.allUsers {
			all, err := q.AnalyticsAllCodexGauges(gctx)
			if err != nil {
				return err
			}
			all, truncated = capGauge(all)
			rows = make([]capacityCodexRow, 0, len(all))
			for _, r := range all {
				rows = append(rows, capacityCodexRow{
					userID: r.UserID, email: r.Email, vaultMode: r.VaultMode, accountID: r.ProviderAccountID, aliases: r.Aliases,
					isDefault: r.IsDefault, reauthRequired: r.ReauthRequired, reauthReason: r.ReauthReason, buckets: r.Buckets,
					lastSuccessAt: r.LastSuccessAt, lastAttemptAt: r.LastAttemptAt,
				})
			}
			return nil
		}
		own, err := q.AnalyticsOwnerCodexGauges(gctx, req.scopeUser)
		if err != nil {
			return err
		}
		own, truncated = capGauge(own)
		rows = make([]capacityCodexRow, 0, len(own))
		for _, r := range own {
			rows = append(rows, capacityCodexRow{
				userID: req.scopeUser, accountID: r.ProviderAccountID, aliases: r.Aliases,
				isDefault: r.IsDefault, reauthRequired: r.ReauthRequired, reauthReason: r.ReauthReason, buckets: r.Buckets,
				lastSuccessAt: r.LastSuccessAt, lastAttemptAt: r.LastAttemptAt,
			})
		}
		return nil
	})
	if err != nil {
		return set, err
	}
	if gaugeErr != nil {
		slog.Warn("analytics capacity: codex gauge unavailable", "error", gaugeErr)
		set.failed = true
		return set, nil
	}
	set.rows, set.truncated = rows, truncated
	return set, nil
}

// finishCapacity runs after the snapshot: it observes Codex vault state, completes the Codex
// gauge and writes the response. A cancelled request is never answered.
func (h *Handler) finishCapacity(w http.ResponseWriter, r *http.Request, d *analyticsDeps, req analyticsRequest, res *capacityResult, now time.Time) {
	if r.Context().Err() != nil {
		return
	}
	locked, observed := h.observeCodexVault(r.Context(), d, req, res.codex.rows)
	if r.Context().Err() != nil {
		return
	}
	res.dto.Gauges.Codex = h.codexGauge(req, res.codex, locked, observed, now)
	httpx.JSON(w, http.StatusOK, res.dto)
}

// observeCodexVault returns each user's lock state for the selected Codex identities. observed
// is false when the state could not be read, in which case the Codex gauge is unavailable.
//
// The owner route reads the caller's own state with RequiresUnlock, exactly as
// SelfCodexRateLimits does. The admin routes read each distinct user serially with the
// non-waiting adminRateVaultTrySnapshot under one budget, and stop at the first user whose state
// is contended, failed or past the budget.
func (h *Handler) observeCodexVault(ctx context.Context, d *analyticsDeps, req analyticsRequest, rows []capacityCodexRow) (map[uuid.UUID]bool, bool) {
	locked := map[uuid.UUID]bool{}
	if len(rows) == 0 {
		return locked, true
	}
	if !req.admin {
		locked[req.scopeUser] = h.vault.RequiresUnlock(ctx, req.scopeUser)
		return locked, true
	}
	budget := capacityVaultObserveBudget
	if d.vaultBudget > 0 {
		budget = d.vaultBudget
	}
	bctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for _, row := range rows {
		if _, seen := locked[row.userID]; seen {
			continue
		}
		_, isLocked, ok, err := h.adminRateVaultTrySnapshot(bctx, row.userID, row.vaultMode)
		// A lookup that returns success after the budget ran out is not accepted: the budget is
		// checked after each lookup as well as before the next one.
		if err == nil && ok && bctx.Err() != nil {
			ok = false
		}
		if err != nil || !ok {
			if err != nil {
				slog.Warn("analytics capacity: vault status unavailable", "user", row.userID, "error", err)
			}
			return nil, false
		}
		locked[row.userID] = isLocked
	}
	return locked, true
}

// adminRateVaultTrySnapshot is adminRateVaultSnapshot that never waits for the user's lifecycle
// boundary: ok is false when another lifecycle operation holds it. Only fixtures without a core
// use the conservative session fallback.
func (h *Handler) adminRateVaultTrySnapshot(ctx context.Context, ownerID uuid.UUID, fallbackMode string) (mode string, locked, ok bool, err error) {
	if h.vault == nil {
		status := h.vaultStatus(ctx, store.User{ID: ownerID, VaultMode: fallbackMode})
		return status.Mode, status.Mode == store.VaultModeProtected && !status.Unlocked, true, nil
	}
	status, ok, err := h.vault.TryStatus(ctx, ownerID)
	if err != nil || !ok {
		return "", false, false, err
	}
	return status.Mode, status.Mode == store.VaultModeProtected && !status.Unlocked, true, nil
}

// codexGauge builds the Codex gauge from its rows and the observed lock states.
func (h *Handler) codexGauge(req analyticsRequest, set capacityCodexSet, locked map[uuid.UUID]bool, observed bool, now time.Time) apitypes.AnalyticsCapacityCodexGaugeDTO {
	out := apitypes.AnalyticsCapacityCodexGaugeDTO{
		VaultStateBasis: capacityVaultStateBasis,
		Identities:      []apitypes.AnalyticsCapacityCodexIdentityDTO{},
	}
	switch {
	case !set.selected:
		out.AnalyticsCapacityGaugeBaseDTO = capacityGaugeBase(req, capacityProviderCodex, capacityGaugeNotSelected, capacityReason(store.AnalyticsReasonNotSelected))
		return out
	case set.failed:
		out.AnalyticsCapacityGaugeBaseDTO = capacityGaugeBase(req, capacityProviderCodex, capacityGaugeUnavailable, capacityReason(store.AnalyticsReasonGaugeUnavailable))
		return out
	case !observed:
		out.AnalyticsCapacityGaugeBaseDTO = capacityGaugeBase(req, capacityProviderCodex, capacityGaugeUnavailable, capacityReason(store.AnalyticsReasonVaultStatusUnavailable))
		return out
	}
	for _, r := range set.rows {
		id := apitypes.AnalyticsCapacityCodexIdentityDTO{
			CodexAccountRateLimitDTO: h.codexAccountDTO(r.accountID, r.aliases, r.isDefault, r.reauthRequired, r.reauthReason,
				r.buckets, r.lastSuccessAt, r.lastAttemptAt, locked[r.userID], now),
		}
		if req.allUsers {
			uid, email := r.userID.String(), r.email
			id.UserID, id.Email = &uid, &email
		}
		out.Identities = append(out.Identities, id)
	}
	out.AnalyticsCapacityGaugeBaseDTO = capacityGaugeBase(req, capacityProviderCodex, capacityGaugeAvailable, nil)
	out.Truncated = set.truncated
	out.SetState = capacitySetState(len(out.Identities), set.truncated)
	return out
}

// mapAnalyticsCapacity builds the historical part of the response: everything except the gauges.
func mapAnalyticsCapacity(req analyticsRequest, b analytics.Bounds, doc store.AnalyticsCapacityDoc) apitypes.AnalyticsCapacityDTO {
	pop := doc.Population
	out := apitypes.AnalyticsCapacityDTO{
		AnalyticsEnvelopeDTO: analyticsEnvelope(req, b),
		SelectedUser:         req.scope().User,
		Filters: apitypes.AnalyticsCapacityFiltersDTO{
			Kinds: slices.DeleteFunc(slices.Clone(req.kinds), func(k string) bool { return k == runkind.Chat }),
		},
		Occupancy: apitypes.AnalyticsCapacityOccupancyDTO{
			TimeBasis: capacityTimeBasisOccupancy,
			Unit:      capacityUnitOccupancy,
			Population: apitypes.AnalyticsCapacityPopulationDTO{
				Candidates: pop.Candidates, WithInterval: pop.WithInterval, WithoutStart: pop.WithoutStart,
				InvalidInterval: pop.InvalidInterval, EndFromStatusSince: pop.EndFromStatusSince, Ongoing: pop.Ongoing,
				CrossCheckKindIncluded: pop.CrossCheckKindIncluded,
			},
			Limitations: capacityLimitations(pop),
			Window:      occupancyCell(doc.Occupancy.Window),
			Buckets:     make([]apitypes.AnalyticsCapacityOccupancyBucketDTO, 0, len(doc.Occupancy.Buckets)),
		},
		UsageWait: apitypes.AnalyticsCapacityUsageWaitDTO{
			TimeBasis: capacityTimeBasisUsageWait,
			Cohort:    capacityCohortUsageWait,
			Basis:     capacityBasisUsageWait,
			Current:   usageWaitCell(doc.UsageWait.Window),
			Buckets:   make([]apitypes.AnalyticsCapacityUsageWaitBucketDTO, 0, len(doc.UsageWait.Buckets)),
		},
		NotTracked: capacityNotTracked(),
	}
	if req.repo != uuid.Nil {
		repo := req.repo.String()
		out.Filters.Repo = &repo
	}
	if req.harness != "" {
		harness := req.harness
		out.Filters.Harness = &harness
	}
	if req.kindName == capacityExcludedKindSelected {
		excluded := capacityExcludedKindSelected
		out.ExcludedKindSelected = &excluded
	}
	for _, c := range doc.Occupancy.Buckets {
		out.Occupancy.Buckets = append(out.Occupancy.Buckets, apitypes.AnalyticsCapacityOccupancyBucketDTO{Index: c.Index, AnalyticsCapacityOccupancyCellDTO: occupancyCell(c)})
	}
	for _, c := range doc.UsageWait.Buckets {
		out.UsageWait.Buckets = append(out.UsageWait.Buckets, apitypes.AnalyticsCapacityUsageWaitBucketDTO{Index: c.Index, AnalyticsCapacityUsageWaitCellDTO: usageWaitCell(c)})
	}
	return out
}

func occupancyCell(c store.AnalyticsCapacityOccupancyCell) apitypes.AnalyticsCapacityOccupancyCellDTO {
	return apitypes.AnalyticsCapacityOccupancyCellDTO{
		RunSeconds: c.RunSeconds, DurationSeconds: c.DurationSeconds, AverageConcurrentRuns: c.AverageConcurrentRuns, Reason: c.Reason,
	}
}

func usageWaitCell(c store.AnalyticsCapacityUsageWaitCell) apitypes.AnalyticsCapacityUsageWaitCellDTO {
	return apitypes.AnalyticsCapacityUsageWaitCellDTO{Count: c.Count, N: c.N, Share: c.Share, Reason: c.Reason}
}

// capacityLimitations is the closed, ordered list of disclosed limits of the occupancy series.
// A code with a count applies to that many candidate runs; the others describe the method.
func capacityLimitations(p store.AnalyticsCapacityPopulation) []apitypes.AnalyticsCapacityLimitationDTO {
	counted := func(code string, n int64) apitypes.AnalyticsCapacityLimitationDTO {
		return apitypes.AnalyticsCapacityLimitationDTO{Code: code, Runs: &n}
	}
	method := func(code string) apitypes.AnalyticsCapacityLimitationDTO {
		return apitypes.AnalyticsCapacityLimitationDTO{Code: code}
	}
	return []apitypes.AnalyticsCapacityLimitationDTO{
		method("elapsed_envelope_includes_waits"),
		method("claim_and_resume_history_not_recovered"),
		method("lane_assignment_history_limited"),
		counted("missing_start_excluded", p.WithoutStart),
		counted("invalid_interval_excluded", p.InvalidInterval),
		counted("end_from_status_since", p.EndFromStatusSince),
		counted("ongoing_end_is_generated_at", p.Ongoing),
		counted("cross_check_kind_not_on_lane_included", p.CrossCheckKindIncluded),
	}
}

// capacityNotTracked lists the measures this endpoint does not report.
func capacityNotTracked() []apitypes.AnalyticsCapacityNotTrackedDTO {
	out := []apitypes.AnalyticsCapacityNotTrackedDTO{}
	for _, m := range []string{"historical_provider_headroom", "slot_count", "utilization", "parked_duration"} {
		out = append(out, apitypes.AnalyticsCapacityNotTrackedDTO{Measure: m, Reason: store.AnalyticsReasonNotTracked})
	}
	return out
}
