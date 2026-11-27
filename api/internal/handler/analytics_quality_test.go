package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4730: Quality handler tests that need no database. They reuse the counting beginner and
// request helpers of analytics_test.go.

var qualityRoutes = []analyticsRoute{
	{"owner quality", "/api/analytics/quality", false, analytics.SectionQuality},
	{"admin quality", "/api/admin/analytics/quality", true, analytics.SectionQuality},
}

func qualityServe(rt analyticsRoute, h *Handler, rec http.ResponseWriter, req *http.Request) {
	if rt.admin {
		h.AdminAnalyticsQuality(rec, req)
		return
	}
	h.AnalyticsQuality(rec, req)
}

func TestAnalyticsQualitySyntaxRejectionsBeginNoTransaction(t *testing.T) {
	someUUID := uuid.New().String()
	owner, admin := qualityRoutes[0], qualityRoutes[1]
	cases := []struct {
		name  string
		route analyticsRoute
		query string
		want  string
	}{
		{"owner split", owner, "split=harness", "invalid split"},
		{"owner breakdown", owner, "breakdown=kind", "invalid breakdown"},
		{"admin split", admin, "split=user", "invalid split"},
		{"admin breakdown", admin, "breakdown=user&user=" + someUUID, "invalid breakdown"},
		{"owner user", owner, "user=" + someUUID, "user not allowed"},
		{"owner user=all", owner, "user=all", "user not allowed"},
		{"owner unknown", owner, "bogus=1", "unknown parameter"},
		{"admin unknown", admin, "bogus=1", "unknown parameter"},
		{"admin all-users window=all", admin, "window=all", msgAnalyticsWindowAllAllUsers},
		{"admin user=all window=all", admin, "window=all&user=all", msgAnalyticsWindowAllAllUsers},
		{"bad window", owner, "window=1y", "invalid window"},
		{"bad tz", owner, "tz=Local", "invalid tz"},
		{"bad repo", owner, "repo=nope", "invalid repo"},
		{"bad harness", admin, "harness=gemini", "invalid harness"},
		{"bad kind", owner, "kind=reliability", "invalid kind"},
		{"bad user", admin, "user=nobody", "invalid user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &countingBeginner{err: errors.New("must not begin")}
			h := newAnalyticsUnitHandler(t, b)
			rec := httptest.NewRecorder()
			qualityServe(tc.route, h, rec, analyticsGet(store.User{ID: uuid.New(), IsAdmin: tc.route.admin}, tc.route.path+"?"+tc.query))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("= %d %s, want 400 naming %q", rec.Code, rec.Body.String(), tc.want)
			}
			if got := b.calls.Load(); got != 0 {
				t.Fatalf("BeginTx called %d times for a syntax error, want 0", got)
			}
		})
	}
}

// The all-users window=all refusal is one response whether user is omitted or user=all.
func TestAnalyticsQualityAdminAllUsersWindowAllRefusalIsIdentical(t *testing.T) {
	rt := qualityRoutes[1]
	var bodies []string
	for _, q := range []string{"window=all", "window=all&user=all"} {
		h := newAnalyticsUnitHandler(t, &countingBeginner{err: errors.New("must not begin")})
		rec := httptest.NewRecorder()
		qualityServe(rt, h, rec, analyticsGet(store.User{ID: uuid.New(), IsAdmin: true}, rt.path+"?"+q))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", q, rec.Code)
		}
		bodies = append(bodies, rec.Body.String())
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &e); err != nil {
		t.Fatal(err)
	}
	if bodies[0] != bodies[1] || e.Error != "window all not available for all users" {
		t.Fatalf("bodies = %q vs %q, want the identical documented refusal", bodies[0], bodies[1])
	}
}

func TestAnalyticsQualityAcceptedQueriesReachTheSnapshot(t *testing.T) {
	someUUID := uuid.New().String()
	owner, admin := qualityRoutes[0], qualityRoutes[1]
	for _, tc := range []struct {
		route analyticsRoute
		query string
	}{
		{owner, ""},
		{owner, "window=all&tz=America/New_York&harness=codex&kind=delivery&repo=" + someUUID},
		{admin, ""},
		{admin, "window=90d&user=all"},
		{admin, "window=all&user=" + someUUID},
	} {
		b := &countingBeginner{err: errors.New("stop here")}
		h := newAnalyticsUnitHandler(t, b)
		qualityServe(tc.route, h, httptest.NewRecorder(), analyticsGet(store.User{ID: uuid.New(), IsAdmin: tc.route.admin}, tc.route.path+"?"+tc.query))
		if b.calls.Load() != 1 {
			t.Fatalf("%s ?%s: BeginTx called %d times, want 1", tc.route.name, tc.query, b.calls.Load())
		}
	}
}

// Quality admits under its own section key: an in-flight Spend of the same caller does not block
// it, and an in-flight Quality blocks the same caller's other route (owner and admin share the key).
func TestAnalyticsQualityAdmissionKeyIsSectionQuality(t *testing.T) {
	b := &countingBeginner{err: errors.New("stop here")}
	h := newAnalyticsUnitHandler(t, b)
	admin := store.User{ID: uuid.New(), IsAdmin: true}

	releaseSpend, err := h.analyticsDeps.admission.Acquire(admin.ID.String(), analytics.SectionSpend)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSpend()
	h.AdminAnalyticsQuality(httptest.NewRecorder(), analyticsGet(admin, "/api/admin/analytics/quality"))
	if b.calls.Load() != 1 {
		t.Fatalf("quality while spend held: BeginTx calls = %d, want 1", b.calls.Load())
	}

	releaseQuality, err := h.analyticsDeps.admission.Acquire(admin.ID.String(), "quality")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseQuality()
	rec := httptest.NewRecorder()
	h.AnalyticsQuality(rec, analyticsGet(admin, "/api/analytics/quality"))
	if rec.Code != http.StatusServiceUnavailable || b.calls.Load() != 1 {
		t.Fatalf("quality while quality held = %d, calls %d, want 503 and 1", rec.Code, b.calls.Load())
	}
}

func TestAnalyticsQualityBusyIs503WithRetryAfterAndNoTransaction(t *testing.T) {
	for _, rt := range qualityRoutes {
		t.Run(rt.name, func(t *testing.T) {
			b := &countingBeginner{err: errors.New("must not begin")}
			h := newAnalyticsUnitHandler(t, b)
			user := store.User{ID: uuid.New(), IsAdmin: rt.admin}
			release, err := h.analyticsDeps.admission.Acquire(user.ID.String(), "quality")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			rec := httptest.NewRecorder()
			qualityServe(rt, h, rec, analyticsGet(user, rt.path))
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" || b.calls.Load() != 0 {
				t.Fatalf("= %d Retry-After %q calls %d, want 503, 1 and 0", rec.Code, rec.Header().Get("Retry-After"), b.calls.Load())
			}
		})
	}
}

func TestAnalyticsQualityStatementTimeoutIs503WithRetryAfterAndReleases(t *testing.T) {
	for _, rt := range qualityRoutes {
		t.Run(rt.name, func(t *testing.T) {
			b := &countingBeginner{err: &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}}
			h := newAnalyticsUnitHandler(t, b)
			user := store.User{ID: uuid.New(), IsAdmin: rt.admin}
			for i := 1; i <= 2; i++ {
				rec := httptest.NewRecorder()
				qualityServe(rt, h, rec, analyticsGet(user, rt.path))
				if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
					t.Fatalf("attempt %d: = %d Retry-After %q, want 503 and 1", i, rec.Code, rec.Header().Get("Retry-After"))
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

func mustQualityBounds(t *testing.T) analytics.Bounds {
	t.Helper()
	b, err := analytics.ComputeBounds(time.Date(2031, 6, 15, 12, 0, 0, 0, time.UTC), time.UTC, analytics.Window7d, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An owner request and an admin user=<uuid> request use the owner statement, scoped to the
// selected user; admin all-users uses the all-scope statement.
func TestAnalyticsQualityStatementScope(t *testing.T) {
	caller, selected := uuid.New(), uuid.New()
	b := mustQualityBounds(t)

	owner := buildQualityStatement(mustParseAnalytics(t, "", false, analytics.SectionQuality, caller), b)
	if owner.owner == nil || owner.all != nil || owner.owner.UserID != caller {
		t.Fatalf("owner route must use the owner statement for the caller: %+v", owner)
	}
	byUser := buildQualityStatement(mustParseAnalytics(t, "user="+selected.String(), true, analytics.SectionQuality, caller), b)
	if byUser.owner == nil || byUser.owner.UserID != selected {
		t.Fatalf("admin user=<uuid> must use the owner statement for the selected user: %+v", byUser)
	}
	for _, q := range []string{"", "user=all"} {
		all := buildQualityStatement(mustParseAnalytics(t, q, true, analytics.SectionQuality, caller), b)
		if all.all == nil || all.owner != nil {
			t.Fatalf("admin %q must use the all-scope statement: %+v", q, all)
		}
	}
	p := owner.owner
	if len(p.BucketStarts) != len(b.BucketStarts) || len(p.BucketEnds) != len(b.BucketEnds) || len(p.AlwaysSkipOrigins) == 0 {
		t.Fatalf("statement parameters incomplete: %+v", p)
	}
	for i := range b.BucketStarts {
		if !p.BucketStarts[i].Valid || !p.BucketStarts[i].Time.Equal(b.BucketStarts[i]) {
			t.Errorf("BucketStarts[%d] = %v, want %v", i, p.BucketStarts[i], b.BucketStarts[i])
		}
		if !p.BucketEnds[i].Valid || !p.BucketEnds[i].Time.Equal(b.BucketEnds[i]) {
			t.Errorf("BucketEnds[%d] = %v, want %v", i, p.BucketEnds[i], b.BucketEnds[i])
		}
	}
	if want := []string{runkind.Issue, runkind.CIFix}; !slices.Equal(p.JudgeKinds, want) {
		t.Fatalf("judge kinds = %v, want %v", p.JudgeKinds, want)
	}
	if !slices.Equal(p.DeliveryKinds, runkind.Delivery()) {
		t.Fatalf("delivery kinds = %v", p.DeliveryKinds)
	}
}

func TestMapAnalyticsQualityEmptyDocHasNoNilSlices(t *testing.T) {
	req := mustParseAnalytics(t, "window=7d", false, analytics.SectionQuality, uuid.New())
	dto := mapAnalyticsQuality(req, mustQualityBounds(t), store.AnalyticsQualityDoc{})
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// Only the documented nullable leaves may be null; every array must be [].
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				walk(path+"."+k, c)
			}
		case nil:
			switch {
			case path == ".history_start", strings.HasSuffix(path, ".reason"), strings.HasSuffix(path, ".value"),
				path == ".cross_check.pending.effective_filters.repo", path == ".cross_check.pending.effective_filters.harness":
			default:
				t.Errorf("unexpected null at %s", path)
			}
		}
	}
	walk("", m)
	if got := string(mustRaw(t, raw, "rolling")); got != "[]" {
		t.Fatalf("rolling = %s, want []", got)
	}
	if dto.Rework.Buckets == nil || dto.CIFix.Buckets == nil || dto.CrossCheck.Plan.Buckets == nil ||
		dto.CrossCheck.Code.Buckets == nil || dto.Judge.Buckets == nil || dto.Judge.Limitations == nil ||
		dto.CrossCheck.Pending.EffectiveFilters.Kinds == nil || dto.Rolling == nil {
		t.Fatal("a slice is nil, which would serialize as JSON null")
	}
	if dto.PreviousFrom != nil || dto.PreviousTo != nil {
		t.Fatal("quality carries no previous window")
	}
	if dto.SelectedUser != req.scopeUser.String() || dto.Scope.Route != "owner" {
		t.Fatalf("scope = %+v selected %q", dto.Scope, dto.SelectedUser)
	}
	if dto.Judge.Coverage.Value != nil || dto.Judge.Coverage.Reason == nil || *dto.Judge.Coverage.Reason != "no_applicable_runs" {
		t.Fatalf("empty judge coverage = %+v, want null with no_applicable_runs", dto.Judge.Coverage)
	}
	for name, n := range map[string]*float64{"merge_day_activity": dto.Rework.MergeDayActivity.Value, "run_end_to_merge_seconds": dto.Rework.RunEndToMergeSeconds.Value} {
		if n != nil {
			t.Fatalf("%s must be null", name)
		}
	}
	if r := dto.Rework.RunEndToMergeSeconds.Reason; r == nil || *r != "not_tracked" {
		t.Fatalf("run_end_to_merge_seconds reason = %v, want not_tracked", r)
	}
	if dto.Rework.Retention != "retained_runs_only" || dto.Judge.Selection != "retained_target_properties" ||
		dto.CrossCheck.Plan.Undated.Window != "not_applied" || dto.CrossCheck.Pending.Window != "not_applied" {
		t.Fatalf("fixed strings wrong: %+v", dto)
	}
}

func mustRaw(t *testing.T, raw []byte, key string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m[key]
}

func TestMapAnalyticsQualityApplicabilityFollowsTheKindFilter(t *testing.T) {
	type app struct{ rework, ciFix, plan, code, pending, judge bool }
	for _, tc := range []struct {
		query string
		want  app
	}{
		{"", app{true, true, true, true, true, true}},
		{"kind=issue", app{true, false, true, true, true, true}},
		{"kind=ci_fix", app{true, true, true, true, true, true}},
		{"kind=task", app{true, false, false, true, true, false}},
		{"kind=judge", app{false, false, false, false, false, false}},
		{"kind=cross_check", app{false, false, false, false, false, false}},
		{"kind=chat", app{false, false, false, false, false, false}},
		{"kind=supporting", app{false, false, false, false, false, false}},
		{"kind=delivery", app{true, true, true, true, true, true}},
	} {
		req := mustParseAnalytics(t, tc.query, false, analytics.SectionQuality, uuid.New())
		dto := mapAnalyticsQuality(req, mustQualityBounds(t), store.AnalyticsQualityDoc{})
		got := app{
			dto.Rework.Applicable, dto.CIFix.Applicable, dto.CrossCheck.Plan.Applicable, dto.CrossCheck.Code.Applicable,
			dto.CrossCheck.Pending.Applicable, dto.Judge.Applicable,
		}
		if got != tc.want {
			t.Fatalf("?%s applicability = %+v, want %+v", tc.query, got, tc.want)
		}
		for name, sec := range map[string]struct {
			applicable bool
			reason     *string
		}{
			"rework": {dto.Rework.Applicable, dto.Rework.Reason}, "ci_fix": {dto.CIFix.Applicable, dto.CIFix.Reason},
			"plan": {dto.CrossCheck.Plan.Applicable, dto.CrossCheck.Plan.Reason}, "judge": {dto.Judge.Applicable, dto.Judge.Reason},
		} {
			if sec.applicable != (sec.reason == nil) || (sec.reason != nil && *sec.reason != "not_applicable") {
				t.Fatalf("?%s %s: applicable=%v reason=%v, want a reason exactly when not applicable", tc.query, name, sec.applicable, sec.reason)
			}
		}
	}
}

func TestMapAnalyticsQualityMapsTheDocument(t *testing.T) {
	repo := uuid.New()
	req := mustParseAnalytics(t, "repo="+repo.String()+"&harness=codex&kind=issue", false, analytics.SectionQuality, uuid.New())
	var doc store.AnalyticsQualityDoc
	doc.Rework.ApplicablePRs = 3
	doc.Rework.Groups = store.AnalyticsQualityReworkGroups{Zero: 1, Two: 2}
	doc.Rework.Buckets = []store.AnalyticsQualityReworkBucket{{Index: 0, PRs: 3, Groups: store.AnalyticsQualityReworkGroups{Zero: 1, Two: 2}}}
	doc.CrossCheck.Plan.StoredResult.Revise = 4
	doc.CrossCheck.Code.StoredResult.Completed = 5
	doc.CrossCheck.Plan.Undated.Count = 2
	doc.CrossCheck.Pending = store.AnalyticsQualityPending{Total: 3, Plan: 1, Code: 2}
	doc.Judge.Eligible.Total = 4
	doc.Judge.Eligible.Complete.Total = 3
	doc.Judge.Buckets = []store.AnalyticsQualityJudgeBucket{{Index: 0, Eligible: 4, Complete: 3}, {Index: 1}}

	dto := mapAnalyticsQuality(req, mustQualityBounds(t), doc)
	if dto.Rework.Groups.Two != 2 || dto.Rework.Buckets[0].Groups.Zero != 1 || dto.Rework.ApplicablePRs != 3 {
		t.Fatalf("rework = %+v", dto.Rework)
	}
	if dto.CrossCheck.Plan.StoredResult.Revise != 4 || dto.CrossCheck.Code.StoredResult.Completed != 5 || dto.CrossCheck.Plan.Undated.Count != 2 {
		t.Fatalf("cross check stage results not kept apart: %+v", dto.CrossCheck)
	}
	f := dto.CrossCheck.Pending.EffectiveFilters
	if dto.CrossCheck.Pending.Total != 3 || f.Repo == nil || *f.Repo != repo.String() || f.Harness == nil || *f.Harness != "codex" || !slices.Equal(f.Kinds, []string{"issue"}) {
		t.Fatalf("pending = %+v", dto.CrossCheck.Pending)
	}
	if c := dto.Judge.Coverage.Value; c == nil || *c != 0.75 {
		t.Fatalf("judge coverage = %+v, want 0.75", dto.Judge.Coverage)
	}
	if c := dto.Judge.Buckets[0].Coverage.Value; c == nil || *c != 0.75 {
		t.Fatalf("bucket coverage = %+v, want 0.75", dto.Judge.Buckets[0].Coverage)
	}
	if b := dto.Judge.Buckets[1].Coverage; b.Value != nil || b.Reason == nil || *b.Reason != "no_applicable_runs" {
		t.Fatalf("empty bucket coverage = %+v, want null with no_applicable_runs", b)
	}
}
