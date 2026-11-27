package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4684: the scoped usage analytics API (Flow added by issue #4729).
//
//	GET /api/analytics/{options,spend,reliability,capacity,quality,flow}        the caller's own runs (requireUser)
//	GET /api/admin/analytics/{options,spend,reliability,capacity,quality,flow}  admin read group (RequireAdminRO)
//
// Request order: (1) syntax validation with no database access, (2) single-flight admission
// that never blocks, (3) one REPEATABLE READ READ ONLY snapshot holding the timezone check,
// the selected-user and repo authority checks and, for spend, reliability, capacity, quality
// (#4730) and flow (#4729), the history-start read and the statements (reliability: cohort,
// then latest failure; analytics_reliability.go); options reads no history start. Capacity
// (issue #4731) also reads the current provider gauges inside the snapshot, each in a
// savepoint, and observes Codex vault state live after it (analytics_capacity.go).

const (
	analyticsRouteOwner = "owner"
	analyticsRouteAdmin = "admin"
	analyticsUserAll    = "all"
	// analyticsOptionsCap is the number of options returned per list; the statements fetch one more.
	analyticsOptionsCap = 200
	analyticsTZMaxLen   = 64
)

// analyticsDeps are the collaborators of the analytics handlers. They are built only when the
// handler has a pool; a Handler without them answers every analytics request with the sanitized
// 500, and tests inject their own.
type analyticsDeps struct {
	admission *analytics.Admission
	begin     store.AnalyticsBeginner
	tzLoader  func(string) (*time.Location, error)

	// afterFirstRead, when non-nil, runs inside the snapshot right after the history-start
	// read. Tests use it to commit a competing write once the snapshot is fixed.
	afterFirstRead func(ctx context.Context)
	// afterCohortRead, when non-nil, runs inside the Reliability snapshot right after the
	// cohort statement and before the latest-failure read (issue #4685).
	afterCohortRead func(ctx context.Context)

	// beforeGauge, when non-nil, runs inside each selected provider's gauge savepoint, before
	// that provider's statement. db is the savepoint transaction, so a test can run raw SQL on
	// it; a returned error is handled as that gauge read's error. Nil in production.
	beforeGauge func(ctx context.Context, db store.DBTX, provider string) error

	// vaultBudget overrides capacityVaultObserveBudget when positive (tests only).
	vaultBudget time.Duration

	// qualityBeforeSnapshotRead, when non-nil, runs at the very start of the Quality statement
	// function, before the snapshot's first query. Tests use it to commit a write that the
	// response must still see.
	qualityBeforeSnapshotRead func(ctx context.Context)

	// tzSet caches pg_timezone_names after the first successful read (never on failure).
	tzMu  sync.Mutex
	tzSet map[string]struct{}
}

// newAnalyticsDeps sizes admission from the pool's connection limit.
func newAnalyticsDeps(pool *pgxpool.Pool) *analyticsDeps {
	return &analyticsDeps{
		admission: analytics.NewAdmission(pool.Config().MaxConns),
		begin:     pool,
		tzLoader:  time.LoadLocation,
	}
}

func (d *analyticsDeps) ready() bool {
	return d != nil && d.admission != nil && d.begin != nil && d.tzLoader != nil
}

// tzNames returns the database's timezone names, reading them inside the snapshot on first use.
func (d *analyticsDeps) tzNames(ctx context.Context, s *store.AnalyticsSnapshot) (map[string]struct{}, error) {
	d.tzMu.Lock()
	set := d.tzSet
	d.tzMu.Unlock()
	if set != nil {
		return set, nil
	}
	names, err := s.Q.AnalyticsTimezoneNames(ctx)
	if err != nil {
		return nil, err
	}
	set = make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	d.tzMu.Lock()
	d.tzSet = set
	d.tzMu.Unlock()
	return set, nil
}

// analyticsBadRequest is a client error found inside the snapshot; its text is the 400 message.
type analyticsBadRequest string

func (e analyticsBadRequest) Error() string { return string(e) }

// Allowlists. Every value outside them is a 400 naming the parameter.
var (
	analyticsHarnesses = []string{"claude", "codex"}
	analyticsKindSets  = []string{"delivery", "supporting"}
	// Splits and breakdowns; "user" is added for an admin response spanning every user.
	analyticsSplits     = []string{"usage_model", "harness", "repo", "kind"}
	analyticsBreakdowns = []string{"primary_model", "harness", "repo", "kind"}
	analyticsWindows    = []string{"7d", "30d", "90d", "all"}
)

// analyticsRequest is the validated query of one request.
type analyticsRequest struct {
	admin   bool
	section string

	window  analytics.Window
	tzName  string
	repo    uuid.UUID // uuid.Nil: no repo filter
	harness string
	kinds   []string
	// kindName is the raw kind parameter ("" when absent). The capacity filters echo the effective
	// kinds (req.kinds), not this name; kindName feeds only the effective-scope selectors the
	// gauges cannot attribute and excluded_kind_selected.
	kindName string

	split     string
	breakdown string

	// allUsers is true for an admin request spanning every user. Otherwise scopeUser is the
	// user the statements are scoped to: the caller on the owner route, the selected user on
	// an admin request with user=<uuid>.
	allUsers  bool
	scopeUser uuid.UUID
}

// parseAnalyticsRequest validates the query without touching the database. The returned
// message is the 400 text and names the offending parameter; it never echoes the value.
func parseAnalyticsRequest(q url.Values, admin bool, section string, caller uuid.UUID) (analyticsRequest, string) {
	req := analyticsRequest{admin: admin, section: section, scopeUser: caller}

	allowed := []string{"window", "tz", "repo", "harness", "kind"}
	switch section { // capacity takes no grouping controls
	case analytics.SectionSpend:
		allowed = append(allowed, "split", "breakdown")
	case analytics.SectionReliability:
		// Reliability has a breakdown but no split.
		allowed = append(allowed, "breakdown")
	}
	if admin {
		allowed = append(allowed, "user")
	}
	// Sorted so that, with several bad parameters, the first error is always the same one.
	for _, key := range slices.Sorted(maps.Keys(q)) {
		if !slices.Contains(allowed, key) {
			// split and breakdown on /options (split also on /reliability), and user on the owner
			// route, get a message that names them.
			switch key {
			case "user":
				return req, "user not allowed"
			case "split", "breakdown":
				return req, "invalid " + key
			}
			return req, "unknown parameter"
		}
		if len(q[key]) != 1 || q[key][0] == "" {
			return req, "invalid " + key
		}
	}

	req.window = analytics.Window30d
	if v := q.Get("window"); v != "" {
		w, err := analytics.ParseWindow(v)
		if err != nil {
			return req, "invalid window"
		}
		req.window = w
	}

	req.tzName = "UTC"
	if q.Has("tz") {
		req.tzName = q.Get("tz")
		if req.tzName == "Local" || len(req.tzName) > analyticsTZMaxLen {
			return req, "invalid tz"
		}
	}

	if v := q.Get("repo"); v != "" {
		id, ok := parseCanonicalUUID(v)
		if !ok {
			return req, "invalid repo"
		}
		req.repo = id
	}

	if v := q.Get("harness"); v != "" {
		if !slices.Contains(analyticsHarnesses, v) {
			return req, "invalid harness"
		}
		req.harness = v
	}

	req.kinds = runkind.All()
	if v := q.Get("kind"); v != "" {
		req.kindName = v
		switch {
		case v == "delivery":
			req.kinds = runkind.Delivery()
		case v == "supporting":
			req.kinds = runkind.Supporting()
		case slices.Contains(runkind.All(), v):
			req.kinds = []string{v}
		default:
			return req, "invalid kind"
		}
	}

	req.allUsers = admin
	if admin {
		if v := q.Get("user"); v != "" && v != analyticsUserAll {
			id, ok := parseCanonicalUUID(v)
			if !ok {
				return req, "invalid user"
			}
			req.allUsers = false
			req.scopeUser = id
		}
	}

	// Owner decision: admin all-users all-time spend is deferred to a follow-up that adds
	// per-run totals for finished runs; Reliability, Capacity (issue #4731), Quality (#4730) and
	// Flow (#4729) follow the same window choices. Options still accepts and ignores window, so
	// the check is skipped for it.
	if section != analytics.SectionOptions && !slices.Contains(req.windowChoices(), string(req.window)) {
		return req, msgAnalyticsWindowAllAllUsers
	}

	splits, breakdowns := req.splitChoices(), req.breakdownChoices()
	req.split, req.breakdown = "usage_model", "repo"
	if section == analytics.SectionReliability {
		req.breakdown = "primary_model"
	}
	if v := q.Get("split"); v != "" {
		if !slices.Contains(splits, v) {
			return req, "invalid split"
		}
		req.split = v
	}
	if v := q.Get("breakdown"); v != "" {
		if !slices.Contains(breakdowns, v) {
			return req, "invalid breakdown"
		}
		req.breakdown = v
	}
	return req, ""
}

// parseCanonicalUUID accepts only the 36-character hyphenated form.
func parseCanonicalUUID(v string) (uuid.UUID, bool) {
	if len(v) != 36 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(v)
	// The nil UUID is never a real repo or user; treating it as a filter value would read as "no filter".
	return id, err == nil && id != uuid.Nil
}

// windowChoices are the windows this scope may use; the all-time window is not offered for
// an admin response spanning every user.
func (r analyticsRequest) windowChoices() []string {
	w := slices.Clone(analyticsWindows)
	if r.allUsers {
		w = slices.DeleteFunc(w, func(s string) bool { return s == string(analytics.WindowAll) })
	}
	return w
}

// splitChoices and breakdownChoices are the grouping dimensions this scope may use; grouping
// by user spans users, so it exists only for an admin response over every user.
func (r analyticsRequest) splitChoices() []string {
	if r.allUsers {
		return append(slices.Clone(analyticsSplits), "user")
	}
	return slices.Clone(analyticsSplits)
}

func (r analyticsRequest) breakdownChoices() []string {
	if r.allUsers {
		return append(slices.Clone(analyticsBreakdowns), "user")
	}
	return slices.Clone(analyticsBreakdowns)
}

// scope is the response's scope block.
func (r analyticsRequest) scope() apitypes.AnalyticsScopeDTO {
	route := analyticsRouteOwner
	if r.admin {
		route = analyticsRouteAdmin
	}
	user := analyticsUserAll
	if !r.allUsers {
		user = r.scopeUser.String()
	}
	return apitypes.AnalyticsScopeDTO{Route: route, User: user}
}

// AnalyticsOptions serves GET /api/analytics/options for the caller's own scope.
func (h *Handler) AnalyticsOptions(w http.ResponseWriter, r *http.Request) {
	h.serveAnalytics(w, r, false, analytics.SectionOptions)
}

// AnalyticsSpend serves GET /api/analytics/spend for the caller's own scope.
func (h *Handler) AnalyticsSpend(w http.ResponseWriter, r *http.Request) {
	h.serveAnalytics(w, r, false, analytics.SectionSpend)
}

// AnalyticsFlow serves GET /api/analytics/flow for the caller's own scope.
func (h *Handler) AnalyticsFlow(w http.ResponseWriter, r *http.Request) {
	h.serveAnalytics(w, r, false, analytics.SectionFlow)
}

// AdminAnalyticsFlow serves GET /api/admin/analytics/flow.
func (h *Handler) AdminAnalyticsFlow(w http.ResponseWriter, r *http.Request) {
	h.serveAnalytics(w, r, true, analytics.SectionFlow)
}

// AdminAnalyticsOptions serves GET /api/admin/analytics/options.
func (h *Handler) AdminAnalyticsOptions(w http.ResponseWriter, r *http.Request) {
	h.serveAnalytics(w, r, true, analytics.SectionOptions)
}

// AdminAnalyticsSpend serves GET /api/admin/analytics/spend.
func (h *Handler) AdminAnalyticsSpend(w http.ResponseWriter, r *http.Request) {
	h.serveAnalytics(w, r, true, analytics.SectionSpend)
}

// AnalyticsReliability serves GET /api/analytics/reliability for the caller's own scope.
func (h *Handler) AnalyticsReliability(w http.ResponseWriter, r *http.Request) {
	h.serveAnalytics(w, r, false, analytics.SectionReliability)
}

// AdminAnalyticsReliability serves GET /api/admin/analytics/reliability.
func (h *Handler) AdminAnalyticsReliability(w http.ResponseWriter, r *http.Request) {
	h.serveAnalytics(w, r, true, analytics.SectionReliability)
}

// AnalyticsCapacity serves GET /api/analytics/capacity for the caller's own scope.
func (h *Handler) AnalyticsCapacity(w http.ResponseWriter, r *http.Request) {
	h.serveAnalytics(w, r, false, analytics.SectionCapacity)
}

// AdminAnalyticsCapacity serves GET /api/admin/analytics/capacity.
func (h *Handler) AdminAnalyticsCapacity(w http.ResponseWriter, r *http.Request) {
	h.serveAnalytics(w, r, true, analytics.SectionCapacity)
}

func (h *Handler) serveAnalytics(w http.ResponseWriter, r *http.Request, admin bool, section string) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if section == analytics.SectionCapacity {
		// Provider gauges and per-owner identities are private, current state.
		w.Header().Set("Cache-Control", "private, no-store")
	}
	deps := h.analyticsDeps
	if !deps.ready() {
		slog.Error("analytics: dependencies not wired")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := h.clock().UTC().Truncate(time.Microsecond)

	// r.URL.Query() drops pairs it cannot parse (a ';' or bad percent-encoding), so parse
	// directly and refuse the request instead of silently falling back to defaults.
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid query")
		return
	}
	req, msg := parseAnalyticsRequest(query, admin, section, user.ID)
	if msg != "" {
		httpx.Error(w, http.StatusBadRequest, msg)
		return
	}

	release, err := deps.admission.Acquire(user.ID.String(), section)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		httpx.Error(w, http.StatusServiceUnavailable, "analytics busy, retry shortly")
		return
	}
	defer release()

	var body any
	var capacity *capacityResult
	err = store.RunAnalyticsSnapshot(r.Context(), deps.begin, func(ctx context.Context, s *store.AnalyticsSnapshot) error {
		var ferr error
		switch section {
		case analytics.SectionOptions:
			body, ferr = deps.options(ctx, s, req, now)
		case analytics.SectionReliability:
			body, ferr = deps.reliability(ctx, s, req, now)
		case analytics.SectionCapacity:
			capacity, ferr = h.capacity(ctx, deps, s, req, now)
		case analytics.SectionQuality:
			body, ferr = deps.quality(ctx, s, req, now)
		case analytics.SectionFlow:
			body, ferr = deps.flow(ctx, s, req, now)
		default:
			body, ferr = deps.spend(ctx, s, req, now)
		}
		return ferr
	})
	if err != nil {
		analyticsFail(w, r, err)
		return
	}
	if section == analytics.SectionCapacity {
		h.finishCapacity(w, r, deps, req, capacity, now)
		return
	}
	httpx.JSON(w, http.StatusOK, body)
}

// analyticsFail maps a snapshot error to a response. Nothing from err reaches the client.
func analyticsFail(w http.ResponseWriter, r *http.Request, err error) {
	var bad analyticsBadRequest
	if errors.As(err, &bad) {
		httpx.Error(w, http.StatusBadRequest, string(bad))
		return
	}
	if errors.Is(store.ClassifyAnalyticsErr(err), store.ErrAnalyticsTimeout) && r.Context().Err() == nil {
		slog.Warn("analytics: query timed out", "error", err)
		w.Header().Set("Retry-After", "1")
		httpx.Error(w, http.StatusServiceUnavailable, "analytics query timed out, retry shortly")
		return
	}
	if r.Context().Err() != nil {
		// The client went away; there is nobody to answer.
		slog.Debug("analytics: request cancelled", "error", err)
		return
	}
	slog.Error("analytics", "error", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}

// authorize runs the in-snapshot checks shared by Options, Spend, Reliability, Capacity and Quality: the timezone, the selected
// user's existence and the repo filter's authority. A repo that does not exist and one the scope
// cannot see produce the identical 400.
func (d *analyticsDeps) authorize(ctx context.Context, s *store.AnalyticsSnapshot, req analyticsRequest) (*time.Location, error) {
	set, err := d.tzNames(ctx, s)
	if err != nil {
		return nil, err
	}
	loc, err := analytics.ValidateTZ(req.tzName, set, d.tzLoader)
	if err != nil {
		return nil, analyticsBadRequest("invalid tz")
	}
	if req.admin && !req.allUsers {
		exists, err := s.Q.AnalyticsUserExists(ctx, req.scopeUser)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, analyticsBadRequest("invalid user")
		}
	}
	if req.repo != uuid.Nil {
		var ok bool
		if req.allUsers {
			ok, err = s.Q.AnalyticsRepoExists(ctx, req.repo)
		} else {
			ok, err = s.Q.AnalyticsOwnerRepoAccessible(ctx, store.AnalyticsOwnerRepoAccessibleParams{RepoID: req.repo, UserID: req.scopeUser})
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, analyticsBadRequest("invalid repo")
		}
	}
	return loc, nil
}

func (d *analyticsDeps) options(ctx context.Context, s *store.AnalyticsSnapshot, req analyticsRequest, now time.Time) (apitypes.AnalyticsOptionsDTO, error) {
	if _, err := d.authorize(ctx, s, req); err != nil {
		return apitypes.AnalyticsOptionsDTO{}, err
	}
	out := apitypes.AnalyticsOptionsDTO{
		Scope:        req.scope(),
		GeneratedAt:  now,
		SelectedUser: req.scope().User,
		Windows:      req.windowChoices(),
		Kinds:        append(runkind.All(), analyticsKindSets...),
		Harnesses:    slices.Clone(analyticsHarnesses),
		Splits:       req.splitChoices(),
		Breakdowns:   req.breakdownChoices(),
	}

	var repos []apitypes.AnalyticsRepoOptionDTO
	if req.allUsers {
		rows, err := s.Q.AnalyticsAllRepoOptions(ctx)
		if err != nil {
			return out, err
		}
		for _, r := range rows {
			repos = append(repos, apitypes.AnalyticsRepoOptionDTO{ID: r.ID.String(), Path: r.PathWithNamespace})
		}
	} else {
		rows, err := s.Q.AnalyticsOwnerRepoOptions(ctx, req.scopeUser)
		if err != nil {
			return out, err
		}
		for _, r := range rows {
			repos = append(repos, apitypes.AnalyticsRepoOptionDTO{ID: r.ID.String(), Path: r.PathWithNamespace})
		}
	}
	out.Repos.Items, out.Repos.Truncated = capOptions(repos)

	if !req.admin {
		reason := analyticsUsersNotApplicable
		out.UsersReason = &reason
		return out, nil
	}
	rows, err := s.Q.AnalyticsUserOptions(ctx)
	if err != nil {
		return out, err
	}
	users := make([]apitypes.AnalyticsUserOptionDTO, 0, len(rows))
	for _, r := range rows {
		users = append(users, apitypes.AnalyticsUserOptionDTO{ID: r.ID.String(), Email: r.Email})
	}
	items, truncated := capOptions(users)
	out.Users = &apitypes.AnalyticsUserOptionsDTO{Items: items, Truncated: truncated}
	return out, nil
}

// analyticsUsersNotApplicable is the reason the owner route carries no user list.
const analyticsUsersNotApplicable = store.AnalyticsReasonNotApplicable

// capOptions trims a list fetched with one extra row to the cap and reports whether it was cut.
func capOptions[T any](rows []T) ([]T, bool) {
	if len(rows) > analyticsOptionsCap {
		return rows[:analyticsOptionsCap], true
	}
	if rows == nil {
		rows = []T{}
	}
	return rows, false
}

func (d *analyticsDeps) spend(ctx context.Context, s *store.AnalyticsSnapshot, req analyticsRequest, now time.Time) (apitypes.AnalyticsSpendDTO, error) {
	loc, err := d.authorize(ctx, s, req)
	if err != nil {
		return apitypes.AnalyticsSpendDTO{}, err
	}

	earliest, err := d.historyStart(ctx, s, req)
	if err != nil {
		return apitypes.AnalyticsSpendDTO{}, err
	}
	bounds, err := analytics.ComputeBounds(now, loc, req.window, earliest)
	if err != nil {
		return apitypes.AnalyticsSpendDTO{}, fmt.Errorf("compute bounds: %w", err)
	}

	stmt := buildSpendStatement(req, bounds)
	var raw []byte
	if stmt.owner != nil {
		raw, err = s.Q.AnalyticsOwnerSpend(ctx, *stmt.owner)
	} else {
		raw, err = s.Q.AnalyticsAllSpend(ctx, *stmt.all)
	}
	if err != nil {
		return apitypes.AnalyticsSpendDTO{}, err
	}
	doc, err := store.DecodeAnalyticsSpendDoc(raw)
	if err != nil {
		return apitypes.AnalyticsSpendDTO{}, err
	}
	return mapAnalyticsSpend(req, bounds, doc)
}

// historyStart reads the scope's earliest matching run creation instant (nil for an empty scope)
// and then runs the afterFirstRead test hook, the point where the snapshot is fixed.
func (d *analyticsDeps) historyStart(ctx context.Context, s *store.AnalyticsSnapshot, req analyticsRequest) (*time.Time, error) {
	var repo pgtype.UUID
	if req.repo != uuid.Nil {
		repo = pgtype.UUID{Bytes: req.repo, Valid: true}
	}
	var harness pgtype.Text
	if req.harness != "" {
		harness = pgtype.Text{String: req.harness, Valid: true}
	}
	var hist pgtype.Timestamptz
	var err error
	if req.allUsers {
		hist, err = s.Q.AnalyticsAllHistoryStart(ctx, store.AnalyticsAllHistoryStartParams{RepoID: repo, Harness: harness, Kinds: req.kinds})
	} else {
		hist, err = s.Q.AnalyticsOwnerHistoryStart(ctx, store.AnalyticsOwnerHistoryStartParams{UserID: req.scopeUser, RepoID: repo, Harness: harness, Kinds: req.kinds})
	}
	if err != nil {
		return nil, err
	}
	if d.afterFirstRead != nil {
		d.afterFirstRead(ctx)
	}
	if !hist.Valid {
		return nil, nil
	}
	e := hist.Time
	return &e, nil
}

// analyticsSpendStatement is the one Spend statement a request runs: owner-shaped (always
// filtered to one user) or all-scope.
type analyticsSpendStatement struct {
	owner *store.AnalyticsOwnerSpendParams
	all   *store.AnalyticsAllSpendParams
}

// buildSpendStatement picks the statement and fills its parameters. RevealRepos comes only from
// which route was mounted (req.admin), never from the query: the owner route always masks repos
// the caller has no connection to.
func buildSpendStatement(req analyticsRequest, b analytics.Bounds) analyticsSpendStatement {
	var repo pgtype.UUID
	if req.repo != uuid.Nil {
		repo = pgtype.UUID{Bytes: req.repo, Valid: true}
	}
	var harness pgtype.Text
	if req.harness != "" {
		harness = pgtype.Text{String: req.harness, Valid: true}
	}
	var prev pgtype.Timestamptz
	if b.PreviousFrom != nil {
		prev = analyticsTS(*b.PreviousFrom)
	}
	if req.allUsers {
		return analyticsSpendStatement{all: &store.AnalyticsAllSpendParams{
			From: analyticsTS(b.From), PreviousFrom: prev, Split: req.split, Breakdown: req.breakdown,
			Now: analyticsTS(b.Now), FoldFrom: analyticsTS(b.FoldFrom),
			BucketStarts: analyticsTSList(b.BucketStarts), BucketEnds: analyticsTSList(b.BucketEnds),
			RollStarts: analyticsTSList(b.RollStarts), RollEnds: analyticsTSList(b.RollEnds),
			RepoID: repo, Harness: harness, Kinds: req.kinds, DeliveryKinds: runkind.Delivery(),
		}}
	}
	return analyticsSpendStatement{owner: &store.AnalyticsOwnerSpendParams{
		From: analyticsTS(b.From), PreviousFrom: prev, Split: req.split, Breakdown: req.breakdown,
		Now: analyticsTS(b.Now), FoldFrom: analyticsTS(b.FoldFrom),
		BucketStarts: analyticsTSList(b.BucketStarts), BucketEnds: analyticsTSList(b.BucketEnds),
		RollStarts: analyticsTSList(b.RollStarts), RollEnds: analyticsTSList(b.RollEnds),
		RevealRepos: req.admin, UserID: req.scopeUser,
		RepoID: repo, Harness: harness, Kinds: req.kinds, DeliveryKinds: runkind.Delivery(),
	}}
}

func analyticsTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func analyticsTSList(ts []time.Time) []pgtype.Timestamptz {
	out := make([]pgtype.Timestamptz, len(ts))
	for i, t := range ts {
		out[i] = analyticsTS(t)
	}
	return out
}

// spendMapper converts the store document to DTOs. The first unparsable dollar amount is kept
// and fails the whole mapping, so a malformed document is a 500 and never a silent zero.
type spendMapper struct{ err error }

// usd parses an exact numeric string. The wire value is a float64; sums over it reconcile with
// the exact figure within 1e-6 USD.
func (m *spendMapper) usd(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		if m.err == nil {
			m.err = fmt.Errorf("analytics: unparsable dollar amount %q", s)
		}
		return 0
	}
	return f
}

func (m *spendMapper) usdPtr(s *string) *float64 {
	if s == nil {
		return nil
	}
	f := m.usd(*s)
	return &f
}

func uuidStringPtr(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

func mapAnalyticsDisclosure(d store.AnalyticsDisclosure) apitypes.AnalyticsDisclosureDTO {
	return apitypes.AnalyticsDisclosureDTO{
		Metered: d.Metered, Subscription: d.Subscription, Unreported: d.Unreported, Missing: d.Missing, Incomplete: d.Incomplete,
	}
}

func (m *spendMapper) block(b store.AnalyticsSpendBlock) apitypes.AnalyticsSpendBlockDTO {
	return apitypes.AnalyticsSpendBlockDTO{
		CostUSD:             m.usd(b.Cost),
		InputTokens:         b.InputTokens,
		CacheReadTokens:     b.CacheReadTokens,
		CacheCreationTokens: b.CacheCreationTokens,
		OutputTokens:        b.OutputTokens,
		Runs:                b.Runs,
		Disclosure:          mapAnalyticsDisclosure(b.Disclosure),
		Coverage: apitypes.AnalyticsCoverageDTO{
			Metered: b.Coverage.Metered, Applicable: b.Coverage.Applicable, Value: b.Coverage.Value, Reason: b.Coverage.Reason,
		},
	}
}

func (m *spendMapper) cells(in []store.AnalyticsSpendCell) []apitypes.AnalyticsSpendCellDTO {
	out := make([]apitypes.AnalyticsSpendCellDTO, 0, len(in))
	for _, c := range in {
		out = append(out, apitypes.AnalyticsSpendCellDTO{Index: c.Index, AnalyticsSpendBlockDTO: m.block(c.AnalyticsSpendBlock)})
	}
	return out
}

func (m *spendMapper) ratio(r store.AnalyticsRatioStat) apitypes.AnalyticsRatioDTO {
	return apitypes.AnalyticsRatioDTO{
		NumeratorUSD: m.usd(r.Numerator),
		Denominator:  r.Denominator,
		Value:        m.usdPtr(r.Value),
		Reason:       r.Reason,
		Disclosure:   mapAnalyticsDisclosure(r.Disclosure),
	}
}

func (m *spendMapper) ratioCells(in []store.AnalyticsRatioCell) []apitypes.AnalyticsRatioCellDTO {
	out := make([]apitypes.AnalyticsRatioCellDTO, 0, len(in))
	for _, c := range in {
		out = append(out, apitypes.AnalyticsRatioCellDTO{Index: c.Index, AnalyticsRatioDTO: m.ratio(c.AnalyticsRatioStat)})
	}
	return out
}

func (m *spendMapper) ratioPanel(p store.AnalyticsRatioPanel) apitypes.AnalyticsRatioPanelDTO {
	out := apitypes.AnalyticsRatioPanelDTO{
		Current: m.ratio(p.Current),
		Buckets: m.ratioCells(p.Buckets),
		Rolling: m.ratioCells(p.Rolling),
	}
	if p.Previous != nil {
		prev := m.ratio(*p.Previous)
		out.Previous = &prev
	}
	return out
}

func mapAnalyticsNumber(n store.AnalyticsNullableNumber) apitypes.AnalyticsNullableNumberDTO {
	return apitypes.AnalyticsNullableNumberDTO{Value: n.Value, Reason: n.Reason}
}

func (m *spendMapper) createdAt(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		if m.err == nil {
			m.err = fmt.Errorf("analytics: unparsable timestamp %q", s)
		}
		return time.Time{}
	}
	return t.UTC()
}

// msgAnalyticsWindowAllAllUsers is the 400 message for an admin all-users window=all request on
// any section except options (Spend, Reliability, Capacity and Quality).
const msgAnalyticsWindowAllAllUsers = "window all not available for all users"

// analyticsEnvelope builds the calendar envelope every analytics response is indexed against.
func analyticsEnvelope(req analyticsRequest, b analytics.Bounds) apitypes.AnalyticsEnvelopeDTO {
	env := apitypes.AnalyticsEnvelopeDTO{
		Scope:           req.scope(),
		GeneratedAt:     b.Now.UTC(),
		Window:          string(req.window),
		Timezone:        req.tzName,
		From:            b.From.UTC(),
		To:              b.Now.UTC(),
		BucketWidthDays: 1,
		Buckets:         make([]apitypes.AnalyticsBucketDTO, 0, len(b.Buckets)),
		Rolling:         make([]apitypes.AnalyticsRollingDTO, 0, len(b.Rolling)),
	}
	if b.WidthWeeks > 0 {
		env.BucketWidthDays = 7 * b.WidthWeeks
	}
	if b.PreviousFrom != nil {
		pf, pt := b.PreviousFrom.UTC(), b.From.UTC()
		env.PreviousFrom, env.PreviousTo = &pf, &pt
	}
	if b.HistoryStart != nil {
		hs := b.HistoryStart.UTC()
		env.HistoryStart = &hs
	}
	for _, bk := range b.Buckets {
		env.Buckets = append(env.Buckets, apitypes.AnalyticsBucketDTO{
			Label: bk.Label.String(), Start: bk.Start.UTC(), End: bk.End.UTC(), Partial: bk.Partial,
		})
	}
	for _, rp := range b.Rolling {
		env.Rolling = append(env.Rolling, apitypes.AnalyticsRollingDTO{Start: rp.Start.UTC(), End: rp.End.UTC(), Partial: rp.Partial})
	}
	return env
}

// mapAnalyticsSpend builds the response from the calendar and the store document.
func mapAnalyticsSpend(req analyticsRequest, b analytics.Bounds, doc store.AnalyticsSpendDoc) (apitypes.AnalyticsSpendDTO, error) {
	m := &spendMapper{}
	out := apitypes.AnalyticsSpendDTO{
		AnalyticsEnvelopeDTO: analyticsEnvelope(req, b),
		SelectedUser:         req.scope().User,
		UntrackedChatRuns:    doc.UntrackedChatRuns,
		Series:               m.cells(doc.Buckets),
		CostPerCompletedRun:  m.ratioPanel(doc.CostPerCompletedRun),
		CostPerMergedPR:      m.ratioPanel(doc.CostPerMergedPR),
		CacheRead: apitypes.AnalyticsCacheReadDTO{
			CacheReadTokens:     doc.CacheRead.CacheReadTokens,
			InputTokens:         doc.CacheRead.InputTokens,
			CacheCreationTokens: doc.CacheRead.CacheCreationTokens,
			Denominator:         doc.CacheRead.Denominator,
			Value:               doc.CacheRead.Value,
			Reason:              doc.CacheRead.Reason,
			Disclosure:          mapAnalyticsDisclosure(doc.CacheRead.Disclosure),
		},
	}
	out.Totals.Current = m.block(doc.Totals.Current)
	if doc.Totals.Previous != nil {
		prev := m.block(*doc.Totals.Previous)
		out.Totals.Previous = &prev
	}

	out.Split = apitypes.AnalyticsSplitDTO{
		Dimension:   doc.Split.Dimension,
		NonAdditive: doc.Split.NonAdditive,
		Groups:      make([]apitypes.AnalyticsSplitGroupDTO, 0, len(doc.Split.Groups)),
	}
	for _, g := range doc.Split.Groups {
		out.Split.Groups = append(out.Split.Groups, apitypes.AnalyticsSplitGroupDTO{
			Rank: g.Rank, IsOther: g.IsOther, Key: g.Key, Label: g.Label, ID: uuidStringPtr(g.ID),
			Totals: m.block(g.Totals), Buckets: m.cells(g.Buckets),
		})
	}

	out.TopRuns = make([]apitypes.AnalyticsTopRunDTO, 0, len(doc.TopRuns))
	for _, t := range doc.TopRuns {
		out.TopRuns = append(out.TopRuns, apitypes.AnalyticsTopRunDTO{
			RunID: t.RunID.String(), Kind: t.Kind, Harness: t.Harness, RunStatus: t.RunStatus, UsageStatus: t.UsageStatus,
			PrimaryModel: t.PrimaryModel, CreatedAt: m.createdAt(t.CreatedAt), CostUSD: m.usd(t.Cost),
			InputTokens: t.InputTokens, CacheReadTokens: t.CacheReadTokens, CacheCreationTokens: t.CacheCreationTokens,
			OutputTokens: t.OutputTokens, RepoKey: t.RepoKey, RepoID: uuidStringPtr(t.RepoID), RepoPath: t.RepoPath,
			UserID: t.UserID.String(), UserEmail: t.UserEmail,
		})
	}

	out.Breakdown = apitypes.AnalyticsBreakdownDTO{
		Dimension: doc.Breakdown.Dimension,
		Rows:      make([]apitypes.AnalyticsBreakdownRowDTO, 0, len(doc.Breakdown.Rows)),
	}
	for _, r := range doc.Breakdown.Rows {
		out.Breakdown.Rows = append(out.Breakdown.Rows, apitypes.AnalyticsBreakdownRowDTO{
			Rank: r.Rank, IsOther: r.IsOther, Key: r.Key, Label: r.Label, ID: uuidStringPtr(r.ID),
			AnalyticsSpendBlockDTO: m.block(r.AnalyticsSpendBlock),
			Finished:               r.Finished,
			Outcomes: apitypes.AnalyticsOutcomesDTO{
				Completed: r.Outcomes.Completed, Cancelled: r.Outcomes.Cancelled, PlanRejected: r.Outcomes.PlanRejected,
				BaseDiverged: r.Outcomes.BaseDiverged, Failed: r.Outcomes.Failed,
			},
			FailureRate:       mapAnalyticsNumber(r.FailureRate),
			CompletionRatio:   mapAnalyticsNumber(r.CompletionRatio),
			CostShare:         mapAnalyticsNumber(r.CostShare),
			ElapsedP50Seconds: mapAnalyticsNumber(r.ElapsedP50Seconds),
			ElapsedP90Seconds: mapAnalyticsNumber(r.ElapsedP90Seconds),
			MergedPRs:         r.MergedPRs,
		})
	}
	if m.err != nil {
		return apitypes.AnalyticsSpendDTO{}, m.err
	}
	return out, nil
}
