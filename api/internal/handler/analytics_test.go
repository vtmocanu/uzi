package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4684: analytics handler tests that need no database. A counting beginner stands in
// for the pool so a request that must stop before the snapshot is proven to begin no transaction.

type countingBeginner struct {
	calls atomic.Int32
	err   error
}

func (b *countingBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	b.calls.Add(1)
	return nil, b.err
}

func newAnalyticsUnitHandler(t *testing.T, begin store.AnalyticsBeginner) *Handler {
	t.Helper()
	h := newRunsHandler(t, &runsStore{})
	h.analyticsDeps = &analyticsDeps{
		admission: analytics.NewAdmission(8),
		begin:     begin,
		tzLoader:  time.LoadLocation,
	}
	return h
}

func analyticsGet(user store.User, target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	return req.WithContext(mw.ContextWithUser(req.Context(), user))
}

type analyticsRoute struct {
	name    string
	path    string
	admin   bool
	section string
}

func (h *Handler) analyticsServe(rt analyticsRoute, rec http.ResponseWriter, req *http.Request) {
	switch {
	case !rt.admin && rt.section == analytics.SectionCapacity:
		h.AnalyticsCapacity(rec, req)
	case rt.admin && rt.section == analytics.SectionCapacity:
		h.AdminAnalyticsCapacity(rec, req)
	case !rt.admin && rt.section == analytics.SectionOptions:
		h.AnalyticsOptions(rec, req)
	case !rt.admin && rt.section == analytics.SectionReliability:
		h.AnalyticsReliability(rec, req)
	case !rt.admin && rt.section == analytics.SectionFlow:
		h.AnalyticsFlow(rec, req)
	case !rt.admin:
		h.AnalyticsSpend(rec, req)
	case rt.section == analytics.SectionOptions:
		h.AdminAnalyticsOptions(rec, req)
	case rt.section == analytics.SectionReliability:
		h.AdminAnalyticsReliability(rec, req)
	case rt.section == analytics.SectionFlow:
		h.AdminAnalyticsFlow(rec, req)
	default:
		h.AdminAnalyticsSpend(rec, req)
	}
}

// The first four entries are indexed positionally by the tests below; reliability routes (#4685)
// are appended after them.
var analyticsRoutes = []analyticsRoute{
	{"owner options", "/api/analytics/options", false, analytics.SectionOptions},
	{"owner spend", "/api/analytics/spend", false, analytics.SectionSpend},
	{"admin options", "/api/admin/analytics/options", true, analytics.SectionOptions},
	{"admin spend", "/api/admin/analytics/spend", true, analytics.SectionSpend},
	{"owner reliability", "/api/analytics/reliability", false, analytics.SectionReliability},
	{"admin reliability", "/api/admin/analytics/reliability", true, analytics.SectionReliability},
	// Issue #4731. Appended, so the indexes above keep their meaning.
	{"owner capacity", "/api/analytics/capacity", false, analytics.SectionCapacity},
	{"admin capacity", "/api/admin/analytics/capacity", true, analytics.SectionCapacity},
	// Issue #4729: appended so the positional indexes above stay valid. The route-wide tests
	// that range over this table (busy, timeout, sanitized 500, unset deps) cover flow too.
	{"owner flow", "/api/analytics/flow", false, analytics.SectionFlow},
	{"admin flow", "/api/admin/analytics/flow", true, analytics.SectionFlow},
}

const (
	analyticsOwnerReliabilityRoute = 4
	analyticsAdminReliabilityRoute = 5
	analyticsOwnerCapacityRoute    = 6
	analyticsAdminCapacityRoute    = 7
	analyticsOwnerFlowRoute        = 8
	analyticsAdminFlowRoute        = 9
)

func TestAnalyticsSyntaxRejectionsBeginNoTransaction(t *testing.T) {
	someUUID := uuid.New().String()
	nilUUID := uuid.Nil.String()
	cases := []struct {
		name  string
		route analyticsRoute
		query string
		want  string
	}{
		{"owner user", analyticsRoutes[1], "user=" + someUUID, "user not allowed"},
		{"owner user=all", analyticsRoutes[1], "user=all", "user not allowed"},
		{"owner options user", analyticsRoutes[0], "user=" + someUUID, "user not allowed"},
		{"owner split=user", analyticsRoutes[1], "split=user", "invalid split"},
		{"owner breakdown=user", analyticsRoutes[1], "breakdown=user", "invalid breakdown"},
		{"admin-uuid split=user", analyticsRoutes[3], "user=" + someUUID + "&split=user", "invalid split"},
		{"admin-uuid breakdown=user", analyticsRoutes[3], "user=" + someUUID + "&breakdown=user", "invalid breakdown"},
		{"bad split", analyticsRoutes[1], "split=nope", "invalid split"},
		{"bad breakdown", analyticsRoutes[1], "breakdown=nope", "invalid breakdown"},
		{"options split", analyticsRoutes[0], "split=harness", "invalid split"},
		{"options breakdown", analyticsRoutes[2], "breakdown=kind", "invalid breakdown"},
		{"unknown param", analyticsRoutes[1], "bogus=1", "unknown parameter"},
		{"admin unknown param", analyticsRoutes[3], "bogus=1", "unknown parameter"},
		{"admin all-users window=all", analyticsRoutes[3], "window=all", msgAnalyticsWindowAllAllUsers},
		{"admin user=all window=all", analyticsRoutes[3], "window=all&user=all", msgAnalyticsWindowAllAllUsers},
		{"bad window", analyticsRoutes[1], "window=1y", "invalid window"},
		{"empty window", analyticsRoutes[1], "window=", "invalid window"},
		{"repeated window", analyticsRoutes[1], "window=7d&window=30d", "invalid window"},
		{"empty tz", analyticsRoutes[1], "tz=", "invalid tz"},
		{"Local tz", analyticsRoutes[1], "tz=Local", "invalid tz"},
		{"long tz", analyticsRoutes[0], "tz=" + strings.Repeat("a", 65), "invalid tz"},
		{"bad repo", analyticsRoutes[1], "repo=not-a-uuid", "invalid repo"},
		{"braced repo", analyticsRoutes[1], "repo=%7B" + someUUID + "%7D", "invalid repo"},
		{"empty repo", analyticsRoutes[1], "repo=", "invalid repo"},
		{"nil repo owner spend", analyticsRoutes[1], "repo=" + nilUUID, "invalid repo"},
		{"nil repo owner options", analyticsRoutes[0], "repo=" + nilUUID, "invalid repo"},
		{"nil repo admin options", analyticsRoutes[2], "repo=" + nilUUID, "invalid repo"},
		{"nil repo admin spend", analyticsRoutes[3], "repo=" + nilUUID, "invalid repo"},
		{"nil user admin spend", analyticsRoutes[3], "user=" + nilUUID, "invalid user"},
		{"nil user admin options", analyticsRoutes[2], "user=" + nilUUID, "invalid user"},
		{"bad harness", analyticsRoutes[1], "harness=gemini", "invalid harness"},
		{"bad kind", analyticsRoutes[1], "kind=reliability", "invalid kind"},
		{"admin bad user", analyticsRoutes[3], "user=nobody", "invalid user"},
		{"admin empty user", analyticsRoutes[3], "user=", "invalid user"},
		{"semicolon pair", analyticsRoutes[1], "window=7d;x=1", "invalid query"},
		{"bad percent-encoding", analyticsRoutes[1], "window=%zz", "invalid query"},
		{"capacity owner user", analyticsRoutes[analyticsOwnerCapacityRoute], "user=" + someUUID, "user not allowed"},
		{"capacity owner split", analyticsRoutes[analyticsOwnerCapacityRoute], "split=repo", "invalid split"},
		{"capacity admin split", analyticsRoutes[analyticsAdminCapacityRoute], "split=repo", "invalid split"},
		{"capacity owner breakdown", analyticsRoutes[analyticsOwnerCapacityRoute], "breakdown=repo", "invalid breakdown"},
		{"capacity owner foo", analyticsRoutes[analyticsOwnerCapacityRoute], "foo=1", "unknown parameter"},
		{"capacity admin foo", analyticsRoutes[analyticsAdminCapacityRoute], "foo=1", "unknown parameter"},
		{"capacity admin window=all omitted user", analyticsRoutes[analyticsAdminCapacityRoute], "window=all", msgAnalyticsWindowAllAllUsers},
		{"capacity admin window=all user=all", analyticsRoutes[analyticsAdminCapacityRoute], "window=all&user=all", msgAnalyticsWindowAllAllUsers},
		{"capacity bad harness", analyticsRoutes[analyticsOwnerCapacityRoute], "harness=gemini", "invalid harness"},
		{"capacity bad kind", analyticsRoutes[analyticsAdminCapacityRoute], "kind=reliability", "invalid kind"},
		{"capacity bad repo", analyticsRoutes[analyticsOwnerCapacityRoute], "repo=nope", "invalid repo"},
		{"capacity bad tz", analyticsRoutes[analyticsOwnerCapacityRoute], "tz=Local", "invalid tz"},
		{"admin options bad user", analyticsRoutes[2], "user=nobody", "invalid user"},
		// Issue #4685: Reliability has a breakdown but no split.
		{"owner reliability split", analyticsRoutes[analyticsOwnerReliabilityRoute], "split=harness", "invalid split"},
		{"admin reliability split", analyticsRoutes[analyticsAdminReliabilityRoute], "split=usage_model", "invalid split"},
		{"admin reliability split=user", analyticsRoutes[analyticsAdminReliabilityRoute], "split=user", "invalid split"},
		{"owner reliability breakdown=user", analyticsRoutes[analyticsOwnerReliabilityRoute], "breakdown=user", "invalid breakdown"},
		{"admin reliability user=<uuid> breakdown=user", analyticsRoutes[analyticsAdminReliabilityRoute], "user=" + someUUID + "&breakdown=user", "invalid breakdown"},
		{"admin reliability window=all", analyticsRoutes[analyticsAdminReliabilityRoute], "window=all", msgAnalyticsWindowAllAllUsers},
		{"admin reliability user=all window=all", analyticsRoutes[analyticsAdminReliabilityRoute], "window=all&user=all", msgAnalyticsWindowAllAllUsers},
	}
	// Every Spend syntax rejection applies unchanged to both Reliability routes (the split
	// ones answer "invalid split" there because the parameter is not accepted at all).
	for _, tc := range slices.Clone(cases) {
		switch tc.route.name {
		case "owner spend":
			tc.route = analyticsRoutes[analyticsOwnerReliabilityRoute]
		case "admin spend":
			tc.route = analyticsRoutes[analyticsAdminReliabilityRoute]
		default:
			continue
		}
		tc.name = "reliability " + tc.name
		cases = append(cases, tc)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &countingBeginner{err: errors.New("must not begin")}
			h := newAnalyticsUnitHandler(t, b)
			rec := httptest.NewRecorder()
			h.analyticsServe(tc.route, rec, analyticsGet(store.User{ID: uuid.New(), IsAdmin: tc.route.admin}, tc.route.path+"?"+tc.query))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d %s, want 400", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body = %s, want it to name %q", rec.Body.String(), tc.want)
			}
			if got := b.calls.Load(); got != 0 {
				t.Fatalf("BeginTx called %d times for a syntax error, want 0", got)
			}
		})
	}
}

// With several bad parameters the 400 message is the same every time.
func TestAnalyticsFirstBadParameterIsDeterministic(t *testing.T) {
	rt := analyticsRoutes[1]
	var first string
	for i := 0; i < 50; i++ {
		h := newAnalyticsUnitHandler(t, &countingBeginner{err: errors.New("must not begin")})
		rec := httptest.NewRecorder()
		h.analyticsServe(rt, rec, analyticsGet(store.User{ID: uuid.New()}, rt.path+"?bogus=1&user=x"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if i == 0 {
			first = rec.Body.String()
			if !strings.Contains(first, "unknown parameter") {
				t.Fatalf("body = %s, want the sorted-first key (bogus) named", first)
			}
		} else if rec.Body.String() != first {
			t.Fatalf("run %d body = %s, want %s", i, rec.Body.String(), first)
		}
	}
}

// The all-users window=all refusal is the same response whether user is omitted or user=all.
func TestAnalyticsAdminAllUsersWindowAllRefusalIsIdentical(t *testing.T) {
	for _, rt := range []analyticsRoute{analyticsRoutes[3], analyticsRoutes[5]} {
		t.Run(rt.name, func(t *testing.T) { checkWindowAllRefusalIsIdentical(t, rt) })
	}
}

func checkWindowAllRefusalIsIdentical(t *testing.T, rt analyticsRoute) {
	t.Helper()
	var bodies []string
	for _, q := range []string{"window=all", "window=all&user=all"} {
		b := &countingBeginner{err: errors.New("must not begin")}
		h := newAnalyticsUnitHandler(t, b)
		rec := httptest.NewRecorder()
		h.analyticsServe(rt, rec, analyticsGet(store.User{ID: uuid.New(), IsAdmin: true}, rt.path+"?"+q))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", q, rec.Code)
		}
		if got := b.calls.Load(); got != 0 {
			t.Fatalf("%s: BeginTx called %d times, want 0", q, got)
		}
		bodies = append(bodies, rec.Body.String())
	}
	if bodies[0] != bodies[1] || !strings.Contains(bodies[0], msgAnalyticsWindowAllAllUsers) {
		t.Fatalf("bodies differ or lack the message: %q vs %q", bodies[0], bodies[1])
	}
	// Pin the documented reason as a literal, independent of the handler's constant.
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &e); err != nil {
		t.Fatalf("decode %q: %v", bodies[0], err)
	}
	if e.Error != "window all not available for all users" {
		t.Fatalf("error = %q, want the documented reason", e.Error)
	}
}

// A well-formed query passes validation and reaches the snapshot (one BeginTx).
func TestAnalyticsAcceptedQueriesReachTheSnapshot(t *testing.T) {
	someUUID := uuid.New().String()
	cases := []struct {
		route analyticsRoute
		query string
	}{
		{analyticsRoutes[1], ""},
		{analyticsRoutes[1], "window=all&tz=America/New_York&harness=codex&kind=delivery&split=repo&breakdown=primary_model"},
		{analyticsRoutes[1], "kind=chat&repo=" + someUUID},
		{analyticsRoutes[0], "window=7d&kind=supporting"},
		{analyticsRoutes[3], "split=user&breakdown=user&user=all"},
		{analyticsRoutes[3], "split=user&breakdown=user"},
		{analyticsRoutes[3], "user=" + someUUID + "&split=usage_model"},
		{analyticsRoutes[2], "user=" + someUUID},
		{analyticsRoutes[3], "window=all&user=" + someUUID},
		{analyticsRoutes[2], "window=all"},
		{analyticsRoutes[2], "window=all&user=all"},
		{analyticsRoutes[3], "window=7d"},
		{analyticsRoutes[3], "window=30d"},
		{analyticsRoutes[3], "window=90d"},
		{analyticsRoutes[3], "window=7d&user=all"},
		{analyticsRoutes[3], "window=30d&user=all"},
		{analyticsRoutes[3], "window=90d&user=all"},
		// Issue #4685: Reliability, including the supporting-only kind filter (it still begins the
		// snapshot, because the repo and user authority checks run before the inapplicable answer).
		{analyticsRoutes[analyticsOwnerReliabilityRoute], ""},
		{analyticsRoutes[analyticsOwnerReliabilityRoute], "window=all&tz=America/New_York&harness=codex&kind=delivery&breakdown=kind"},
		{analyticsRoutes[analyticsOwnerReliabilityRoute], "kind=supporting&repo=" + someUUID},
		{analyticsRoutes[analyticsAdminReliabilityRoute], "breakdown=user"},
		{analyticsRoutes[analyticsAdminReliabilityRoute], "user=" + someUUID + "&window=all&breakdown=harness"},
		{analyticsRoutes[analyticsAdminReliabilityRoute], "window=7d&user=all&kind=chat"},
		{analyticsRoutes[analyticsOwnerCapacityRoute], ""},
		{analyticsRoutes[analyticsOwnerCapacityRoute], "window=all&tz=America/New_York&harness=codex&kind=chat&repo=" + someUUID},
		{analyticsRoutes[analyticsAdminCapacityRoute], "window=7d&user=all"},
		{analyticsRoutes[analyticsAdminCapacityRoute], "window=all&user=" + someUUID},
	}
	for _, tc := range cases {
		b := &countingBeginner{err: errors.New("stop here")}
		h := newAnalyticsUnitHandler(t, b)
		rec := httptest.NewRecorder()
		h.analyticsServe(tc.route, rec, analyticsGet(store.User{ID: uuid.New(), IsAdmin: tc.route.admin}, tc.route.path+"?"+tc.query))
		if b.calls.Load() != 1 {
			t.Fatalf("%s ?%s: BeginTx called %d times, want 1 (status %d %s)", tc.route.name, tc.query, b.calls.Load(), rec.Code, rec.Body.String())
		}
	}
}

func TestAnalyticsBusyIs503WithRetryAfterAndNoTransaction(t *testing.T) {
	for _, rt := range analyticsRoutes {
		t.Run(rt.name, func(t *testing.T) {
			b := &countingBeginner{err: errors.New("must not begin")}
			h := newAnalyticsUnitHandler(t, b)
			user := store.User{ID: uuid.New(), IsAdmin: rt.admin}
			release, err := h.analyticsDeps.admission.Acquire(user.ID.String(), rt.section)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			rec := httptest.NewRecorder()
			h.analyticsServe(rt, rec, analyticsGet(user, rt.path))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d %s, want 503", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Retry-After"); got != "1" {
				t.Fatalf("Retry-After = %q, want 1", got)
			}
			if got := b.calls.Load(); got != 0 {
				t.Fatalf("BeginTx called %d times while busy, want 0", got)
			}
		})
	}
}

// The owner and admin variants of a section share one admission key, and a different section
// does not: an in-flight owner spend blocks the same caller's admin spend, not their options.
func TestAnalyticsAdmissionKeyIsPerCallerAndSection(t *testing.T) {
	b := &countingBeginner{err: errors.New("stop here")}
	h := newAnalyticsUnitHandler(t, b)
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	release, err := h.analyticsDeps.admission.Acquire(admin.ID.String(), analytics.SectionSpend)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	rec := httptest.NewRecorder()
	h.AdminAnalyticsSpend(rec, analyticsGet(admin, "/api/admin/analytics/spend"))
	if rec.Code != http.StatusServiceUnavailable || b.calls.Load() != 0 {
		t.Fatalf("admin spend while owner spend held = %d, calls %d, want 503 and 0", rec.Code, b.calls.Load())
	}
	rec = httptest.NewRecorder()
	h.AdminAnalyticsOptions(rec, analyticsGet(admin, "/api/admin/analytics/options"))
	if b.calls.Load() != 1 {
		t.Fatalf("options while spend held: BeginTx calls = %d, want 1 (status %d)", b.calls.Load(), rec.Code)
	}
	rec = httptest.NewRecorder()
	h.AnalyticsSpend(rec, analyticsGet(store.User{ID: uuid.New()}, "/api/analytics/spend"))
	if b.calls.Load() != 2 {
		t.Fatalf("another caller's spend: BeginTx calls = %d, want 2 (status %d)", b.calls.Load(), rec.Code)
	}
}

func TestAnalyticsStatementTimeoutIs503WithRetryAfterAndReleases(t *testing.T) {
	for _, rt := range analyticsRoutes {
		t.Run(rt.name, func(t *testing.T) {
			b := &countingBeginner{err: &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}}
			h := newAnalyticsUnitHandler(t, b)
			user := store.User{ID: uuid.New(), IsAdmin: rt.admin}
			for i := 1; i <= 2; i++ {
				rec := httptest.NewRecorder()
				h.analyticsServe(rt, rec, analyticsGet(user, rt.path))
				if rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("attempt %d: status = %d %s, want 503", i, rec.Code, rec.Body.String())
				}
				if rec.Header().Get("Retry-After") != "1" {
					t.Fatalf("attempt %d: Retry-After = %q, want 1", i, rec.Header().Get("Retry-After"))
				}
				if strings.Contains(rec.Body.String(), "57014") || strings.Contains(rec.Body.String(), "statement timeout") {
					t.Fatalf("attempt %d: body leaks the database error: %s", i, rec.Body.String())
				}
				// The second attempt reaching BeginTx again proves the first released its slot.
				if got := int(b.calls.Load()); got != i {
					t.Fatalf("attempt %d: BeginTx calls = %d, want %d (slot not released?)", i, got, i)
				}
			}
		})
	}
}

func TestAnalyticsOtherDatabaseErrorIsSanitized500(t *testing.T) {
	const secret = "relation run_usage_secret_table does not exist"
	for _, rt := range analyticsRoutes {
		t.Run(rt.name, func(t *testing.T) {
			h := newAnalyticsUnitHandler(t, &countingBeginner{err: errors.New(secret)})
			rec := httptest.NewRecorder()
			h.analyticsServe(rt, rec, analyticsGet(store.User{ID: uuid.New(), IsAdmin: rt.admin}, rt.path))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "run_usage") || strings.Contains(rec.Body.String(), "relation") {
				t.Fatalf("body leaks the error text: %s", rec.Body.String())
			}
			if rec.Header().Get("Retry-After") != "" {
				t.Fatalf("a non-timeout error must not advertise Retry-After")
			}
		})
	}
}

func TestAnalyticsUnsetDepsIsSanitized500(t *testing.T) {
	for _, rt := range analyticsRoutes {
		t.Run(rt.name, func(t *testing.T) {
			h := newRunsHandler(t, &runsStore{})
			rec := httptest.NewRecorder()
			h.analyticsServe(rt, rec, analyticsGet(store.User{ID: uuid.New(), IsAdmin: true}, rt.path))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d %s, want 500", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "internal error") {
				t.Fatalf("body = %s, want the sanitized message", rec.Body.String())
			}
		})
	}
}

func TestAnalyticsRequiresAnAuthenticatedUser(t *testing.T) {
	h := newAnalyticsUnitHandler(t, &countingBeginner{})
	rec := httptest.NewRecorder()
	h.AnalyticsSpend(rec, httptest.NewRequest(http.MethodGet, "/api/analytics/spend", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func mustParseAnalytics(t *testing.T, query string, admin bool, section string, caller uuid.UUID) analyticsRequest {
	t.Helper()
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	req, msg := parseAnalyticsRequest(q, admin, section, caller)
	if msg != "" {
		t.Fatalf("parse %q: %s", query, msg)
	}
	return req
}

// The owner handler never reveals repos and always scopes to the caller, whatever the query
// says; an admin selecting a user reveals repos for that user; admin-all uses the all-scope
// statement. RevealRepos derives from the route, so no query parameter can flip it.
func TestAnalyticsSpendStatementScopeAndReveal(t *testing.T) {
	caller, selected := uuid.New(), uuid.New()
	bounds, err := analytics.ComputeBounds(time.Date(2031, 6, 15, 12, 0, 0, 0, time.UTC), time.UTC, analytics.Window30d, nil)
	if err != nil {
		t.Fatal(err)
	}

	owner := buildSpendStatement(mustParseAnalytics(t, "kind=issue", false, analytics.SectionSpend, caller), bounds)
	if owner.owner == nil || owner.all != nil {
		t.Fatalf("owner route must use the owner statement: %+v", owner)
	}
	if owner.owner.RevealRepos {
		t.Fatal("owner route passed RevealRepos=true")
	}
	if owner.owner.UserID != caller {
		t.Fatalf("owner statement user = %s, want the caller %s", owner.owner.UserID, caller)
	}

	// A query that tries to smuggle the flag or a user is rejected before any statement exists.
	for _, q := range []string{"reveal_repos=true", "user=" + selected.String()} {
		vals, _ := url.ParseQuery(q)
		if _, msg := parseAnalyticsRequest(vals, false, analytics.SectionSpend, caller); msg == "" {
			t.Fatalf("owner route accepted %q", q)
		}
	}

	byUser := buildSpendStatement(mustParseAnalytics(t, "user="+selected.String(), true, analytics.SectionSpend, caller), bounds)
	if byUser.owner == nil || !byUser.owner.RevealRepos || byUser.owner.UserID != selected {
		t.Fatalf("admin user=<uuid> must use the owner statement for the selected user with reveal: %+v", byUser.owner)
	}

	all := buildSpendStatement(mustParseAnalytics(t, "", true, analytics.SectionSpend, caller), bounds)
	if all.all == nil || all.owner != nil {
		t.Fatalf("admin-all must use the all-scope statement: %+v", all)
	}
	explicit := buildSpendStatement(mustParseAnalytics(t, "user=all", true, analytics.SectionSpend, caller), bounds)
	if explicit.all == nil {
		t.Fatal("explicit user=all must use the all-scope statement")
	}
}

func TestAnalyticsParseDefaultsAndKinds(t *testing.T) {
	caller := uuid.New()
	req := mustParseAnalytics(t, "", false, analytics.SectionSpend, caller)
	if req.window != analytics.Window30d || req.tzName != "UTC" || req.split != "usage_model" || req.breakdown != "repo" {
		t.Fatalf("defaults = window %q tz %q split %q breakdown %q", req.window, req.tzName, req.split, req.breakdown)
	}
	if !slices.Equal(req.kinds, runkind.All()) || !slices.Contains(req.kinds, runkind.Chat) {
		t.Fatalf("no kind filter must cover every kind including chat, got %v", req.kinds)
	}
	for q, want := range map[string][]string{
		"kind=delivery":   runkind.Delivery(),
		"kind=supporting": runkind.Supporting(),
		"kind=job":        {runkind.Job},
		"kind=chat":       {runkind.Chat},
	} {
		if got := mustParseAnalytics(t, q, false, analytics.SectionSpend, caller).kinds; !slices.Equal(got, want) {
			t.Fatalf("%s kinds = %v, want %v", q, got, want)
		}
	}

	admin := mustParseAnalytics(t, "", true, analytics.SectionSpend, caller)
	if !admin.allUsers || admin.scope().User != "all" || admin.scope().Route != "admin" {
		t.Fatalf("omitted admin user scope = %+v, want all", admin.scope())
	}
	if !slices.Contains(admin.splitChoices(), "user") || slices.Contains(req.splitChoices(), "user") {
		t.Fatalf("user grouping must exist for admin-all only: admin %v owner %v", admin.splitChoices(), req.splitChoices())
	}
}

func TestMapAnalyticsSpendDollarsAndEmptySlices(t *testing.T) {
	bounds, err := analytics.ComputeBounds(time.Date(2031, 6, 15, 12, 0, 0, 0, time.UTC), time.UTC, analytics.Window7d, nil)
	if err != nil {
		t.Fatal(err)
	}
	callerID := uuid.New()
	req := mustParseAnalytics(t, "window=7d", false, analytics.SectionSpend, callerID)
	val := "0.250000"
	doc := store.AnalyticsSpendDoc{}
	doc.Totals.Current.Cost = "12.345678"
	doc.CostPerCompletedRun.Current.Numerator = "0.000000"
	doc.CostPerMergedPR.Current = store.AnalyticsRatioStat{Numerator: "5.000000", Denominator: 2, Value: &val}

	dto, err := mapAnalyticsSpend(req, bounds, doc)
	if err != nil {
		t.Fatal(err)
	}
	if dto.SelectedUser != callerID.String() {
		t.Fatalf("selected_user = %q, want the caller %s", dto.SelectedUser, callerID)
	}
	if dto.Totals.Current.CostUSD != 12.345678 {
		t.Fatalf("cost_usd = %v, want 12.345678", dto.Totals.Current.CostUSD)
	}
	if dto.CostPerMergedPR.Current.NumeratorUSD != 5 || dto.CostPerMergedPR.Current.Value == nil || *dto.CostPerMergedPR.Current.Value != 0.25 {
		t.Fatalf("ratio = %+v", dto.CostPerMergedPR.Current)
	}
	if dto.Series == nil || dto.TopRuns == nil || dto.Split.Groups == nil || dto.Breakdown.Rows == nil || dto.Buckets == nil || dto.Rolling == nil {
		t.Fatal("a panel slice is nil, which would serialize as JSON null")
	}
	if dto.Totals.Previous != nil || dto.PreviousFrom == nil || dto.PreviousTo == nil || !dto.PreviousTo.Equal(dto.From) {
		t.Fatalf("previous window: totals %v from %v to %v", dto.Totals.Previous, dto.PreviousFrom, dto.PreviousTo)
	}
	if dto.BucketWidthDays != 1 || dto.Timezone != "UTC" || dto.Scope.Route != "owner" {
		t.Fatalf("envelope = %+v", dto.AnalyticsEnvelopeDTO)
	}

	doc.Totals.Current.Cost = "not-a-number"
	if _, err := mapAnalyticsSpend(req, bounds, doc); err == nil {
		t.Fatal("an unparsable dollar amount must fail the mapping")
	}
}
