package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4729: Flow handler tests that need no database. analyticsRoutes[analyticsOwnerFlowRoute] is the owner flow
// route and [analyticsAdminFlowRoute] the admin one.

func TestAnalyticsFlowParserRejectionsBeginNoTransaction(t *testing.T) {
	someUUID := uuid.New().String()
	ownerFlow, adminFlow := analyticsRoutes[analyticsOwnerFlowRoute], analyticsRoutes[analyticsAdminFlowRoute]
	cases := []struct {
		name  string
		route analyticsRoute
		query string
		want  string
	}{
		{"owner user", ownerFlow, "user=" + someUUID, "user not allowed"},
		{"owner user=all", ownerFlow, "user=all", "user not allowed"},
		{"owner split", ownerFlow, "split=repo", "invalid split"},
		{"owner breakdown", ownerFlow, "breakdown=repo", "invalid breakdown"},
		{"admin split", adminFlow, "split=repo", "invalid split"},
		{"admin split=user", adminFlow, "user=" + someUUID + "&split=user", "invalid split"},
		{"admin breakdown", adminFlow, "breakdown=kind", "invalid breakdown"},
		{"owner unknown", ownerFlow, "bogus=1", "unknown parameter"},
		{"admin unknown", adminFlow, "bogus=1", "unknown parameter"},
		{"admin all-users window=all", adminFlow, "window=all", msgAnalyticsWindowAllAllUsers},
		{"admin user=all window=all", adminFlow, "window=all&user=all", msgAnalyticsWindowAllAllUsers},
		{"bad window", ownerFlow, "window=1y", "invalid window"},
		{"bad kind", ownerFlow, "kind=reliability", "invalid kind"},
		{"bad harness", adminFlow, "harness=gemini", "invalid harness"},
		{"bad repo", ownerFlow, "repo=nope", "invalid repo"},
		{"bad user", adminFlow, "user=nobody", "invalid user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &countingBeginner{err: errors.New("must not begin")}
			h := newAnalyticsUnitHandler(t, b)
			rec := httptest.NewRecorder()
			h.analyticsServe(tc.route, rec, analyticsGet(store.User{ID: uuid.New(), IsAdmin: tc.route.admin}, tc.route.path+"?"+tc.query))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("= %d %s, want 400 naming %q", rec.Code, rec.Body.String(), tc.want)
			}
			if got := b.calls.Load(); got != 0 {
				t.Fatalf("BeginTx called %d times for a syntax error, want 0", got)
			}
		})
	}
}

// Every allowed flow parameter, and window=all for one user, reaches the snapshot.
func TestAnalyticsFlowAcceptedQueriesReachTheSnapshot(t *testing.T) {
	someUUID := uuid.New().String()
	cases := []struct {
		route analyticsRoute
		query string
	}{
		{analyticsRoutes[analyticsOwnerFlowRoute], ""},
		{analyticsRoutes[analyticsOwnerFlowRoute], "window=all&tz=America/New_York&harness=codex&kind=delivery&repo=" + someUUID},
		{analyticsRoutes[analyticsOwnerFlowRoute], "kind=chat"},
		{analyticsRoutes[analyticsAdminFlowRoute], "window=7d&user=all"},
		{analyticsRoutes[analyticsAdminFlowRoute], "window=90d"},
		{analyticsRoutes[analyticsAdminFlowRoute], "window=all&user=" + someUUID},
		{analyticsRoutes[analyticsAdminFlowRoute], "user=" + someUUID + "&kind=issue&harness=claude"},
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

// Options keeps accepting window=all for admin all-users; only spend and flow refuse it.
func TestAnalyticsWindowAllRefusalCoversSpendAndFlowOnly(t *testing.T) {
	caller := uuid.New()
	for section, wantRefused := range map[string]bool{
		analytics.SectionSpend: true, analytics.SectionFlow: true, analytics.SectionOptions: false,
	} {
		_, msg := parseAnalyticsRequest(url.Values{"window": {"all"}}, true, section, caller)
		if (msg == msgAnalyticsWindowAllAllUsers) != wantRefused {
			t.Fatalf("%s: admin all-users window=all message %q, refused want %v", section, msg, wantRefused)
		}
	}
}

// The owner route and an admin user=<uuid> request run the owner statement for the right user;
// only an admin request spanning every user runs the all-scope statement.
func TestAnalyticsFlowStatementScope(t *testing.T) {
	caller, selected, repo := uuid.New(), uuid.New(), uuid.New()
	bounds, err := analytics.ComputeBounds(time.Date(2031, 6, 15, 12, 0, 0, 0, time.UTC), time.UTC, analytics.Window30d, nil)
	if err != nil {
		t.Fatal(err)
	}

	owner := buildFlowStatement(mustParseAnalytics(t, "kind=issue&harness=codex&repo="+repo.String(), false, analytics.SectionFlow, caller), bounds)
	if owner.owner == nil || owner.all != nil || owner.owner.UserID != caller {
		t.Fatalf("owner route must use the owner statement for the caller: %+v", owner)
	}
	p := owner.owner
	if !slices.Equal(p.Kinds, []string{runkind.Issue}) || !p.Harness.Valid || p.Harness.String != "codex" || !p.RepoID.Valid || p.RepoID.Bytes != repo {
		t.Fatalf("owner filters not passed through: %+v", p)
	}
	if p.MinPercentileN != store.FlowMinPercentileN || !slices.Equal(p.HistogramEdges, store.FlowHistogramEdgesSeconds) {
		t.Fatalf("percentile floor / histogram edges = %d / %v", p.MinPercentileN, p.HistogramEdges)
	}
	if len(p.BucketStarts) != len(bounds.Buckets) || len(p.BucketEnds) != len(bounds.Buckets) || !p.Now.Time.Equal(bounds.Now) || !p.From.Time.Equal(bounds.From) {
		t.Fatalf("calendar not passed through: %d starts, now %v from %v", len(p.BucketStarts), p.Now.Time, p.From.Time)
	}

	byUser := buildFlowStatement(mustParseAnalytics(t, "user="+selected.String(), true, analytics.SectionFlow, caller), bounds)
	if byUser.owner == nil || byUser.owner.UserID != selected {
		t.Fatalf("admin user=<uuid> must use the owner statement for the selected user: %+v", byUser)
	}
	for _, q := range []string{"", "user=all"} {
		all := buildFlowStatement(mustParseAnalytics(t, q, true, analytics.SectionFlow, caller), bounds)
		if all.all == nil || all.owner != nil || !slices.Equal(all.all.Kinds, runkind.All()) {
			t.Fatalf("admin %q must use the all-scope statement over every kind: %+v", q, all)
		}
		if all.all.RepoID.Valid || all.all.Harness.Valid {
			t.Fatalf("no filters were given: %+v", all.all)
		}
	}
}

// flowDocFor builds a document whose elapsed buckets match the calendar, every list nil.
func flowDocFor(b analytics.Bounds) store.AnalyticsFlowDoc {
	doc := store.AnalyticsFlowDoc{}
	for i := range b.Buckets {
		doc.IssueElapsed.Buckets = append(doc.IssueElapsed.Buckets, store.AnalyticsFlowElapsedCell{Index: i})
		doc.Activity.Created.Buckets = append(doc.Activity.Created.Buckets, store.AnalyticsFlowCreatedCell{Index: i})
		doc.Activity.Finished.Buckets = append(doc.Activity.Finished.Buckets, store.AnalyticsFlowFinishedCell{Index: i})
		doc.DeliveryFunnel.PRUnits.Buckets = append(doc.DeliveryFunnel.PRUnits.Buckets, store.AnalyticsFlowPRUnitCell{Index: i})
	}
	return doc
}

func flowBounds(t *testing.T, window analytics.Window) analytics.Bounds {
	t.Helper()
	b, err := analytics.ComputeBounds(time.Date(2031, 6, 15, 12, 0, 0, 0, time.UTC), time.UTC, window, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A nil list in the store document is [] on the wire; null appears only where the contract
// allows it (nullable scalars, history_start, the repo / harness echo).
func TestMapAnalyticsFlowNilListsAreEmptyArrays(t *testing.T) {
	b := flowBounds(t, analytics.Window7d)
	req := mustParseAnalytics(t, "window=7d", false, analytics.SectionFlow, uuid.New())
	dto, err := mapAnalyticsFlow(req, b, flowDocFor(b))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	nullable := map[string]bool{"history_start": true, "repo": true, "harness": true, "reason": true, "value": true, "upper_seconds": true}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case nil:
			if key := path[strings.LastIndex(path, ".")+1:]; !nullable[key] {
				t.Errorf("%s is null, want a value or []", path)
			}
		case map[string]any:
			for k, c := range x {
				walk(path+"."+k, c)
			}
		case []any:
			for i, c := range x {
				walk(path+"."+string(rune('0'+i%10)), c)
			}
		}
	}
	walk("", tree)

	for name, got := range map[string]int{
		"waiting_now.states":      len(dto.WaitingNow.States),
		"finished.excluded_kinds": len(dto.Activity.Finished.ExcludedKinds), "first_start.limitations": len(dto.FirstStartDelay.Limitations),
		"histogram": len(dto.IssueElapsed.Histogram),
	} {
		if got != 0 {
			t.Errorf("%s has %d entries from an empty document", name, got)
		}
	}
	if dto.WaitingNow.States == nil || dto.Activity.Finished.ExcludedKinds == nil || dto.FirstStartDelay.Limitations == nil {
		t.Fatal("a flow list is nil, which serializes as JSON null")
	}
}

func TestMapAnalyticsFlowElapsedPartialComesFromTheCalendar(t *testing.T) {
	b := flowBounds(t, analytics.Window7d)
	req := mustParseAnalytics(t, "window=7d", false, analytics.SectionFlow, uuid.New())
	doc := flowDocFor(b)
	// Cells arrive in a different order than the calendar: the index, not the position, selects the bucket.
	slices.Reverse(doc.IssueElapsed.Buckets)
	dto, err := mapAnalyticsFlow(req, b, doc)
	if err != nil {
		t.Fatal(err)
	}
	var partials int
	for _, c := range dto.IssueElapsed.Buckets {
		if c.Partial != b.Buckets[c.Index].Partial {
			t.Fatalf("bucket %d partial = %v, calendar says %v", c.Index, c.Partial, b.Buckets[c.Index].Partial)
		}
		if c.Partial {
			partials++
		}
	}
	if partials != 1 || !dto.IssueElapsed.Buckets[0].Partial {
		t.Fatalf("exactly the last calendar bucket is partial; got %d partial cells, first cell %+v", partials, dto.IssueElapsed.Buckets[0])
	}
}

func TestMapAnalyticsFlowBucketMismatchIsAnError(t *testing.T) {
	b := flowBounds(t, analytics.Window7d)
	req := mustParseAnalytics(t, "window=7d", false, analytics.SectionFlow, uuid.New())

	short := flowDocFor(b)
	short.IssueElapsed.Buckets = short.IssueElapsed.Buckets[:len(short.IssueElapsed.Buckets)-1]
	long := flowDocFor(b)
	long.IssueElapsed.Buckets = append(long.IssueElapsed.Buckets, store.AnalyticsFlowElapsedCell{Index: len(b.Buckets)})
	outOfRange := flowDocFor(b)
	outOfRange.IssueElapsed.Buckets[2].Index = len(b.Buckets)
	negative := flowDocFor(b)
	negative.IssueElapsed.Buckets[0].Index = -1
	empty := store.AnalyticsFlowDoc{}
	// Right count, but index 1 is missing and index 0 is repeated: dropped and duplicated buckets.
	dup := flowDocFor(b)
	dup.IssueElapsed.Buckets[1].Index = 0
	createdDup := flowDocFor(b)
	createdDup.Activity.Created.Buckets[1].Index = 0
	finishedDup := flowDocFor(b)
	finishedDup.Activity.Finished.Buckets[1].Index = 0
	prDup := flowDocFor(b)
	prDup.DeliveryFunnel.PRUnits.Buckets[1].Index = 0
	createdOut := flowDocFor(b)
	createdOut.Activity.Created.Buckets[3].Index = 99
	finishedOut := flowDocFor(b)
	finishedOut.Activity.Finished.Buckets[3].Index = 99
	prOut := flowDocFor(b)
	prOut.DeliveryFunnel.PRUnits.Buckets[3].Index = -5
	createdShort := flowDocFor(b)
	createdShort.Activity.Created.Buckets = createdShort.Activity.Created.Buckets[:3]
	finishedShort := flowDocFor(b)
	finishedShort.Activity.Finished.Buckets = finishedShort.Activity.Finished.Buckets[:3]
	prLong := flowDocFor(b)
	prLong.DeliveryFunnel.PRUnits.Buckets = append(prLong.DeliveryFunnel.PRUnits.Buckets, store.AnalyticsFlowPRUnitCell{Index: 0})

	for name, doc := range map[string]store.AnalyticsFlowDoc{
		"fewer cells": short, "more cells": long, "index past the calendar": outOfRange, "negative index": negative, "no cells": empty,
		"elapsed duplicate index": dup, "created duplicate index": createdDup, "finished duplicate index": finishedDup, "pr_units duplicate index": prDup,
		"created index out of range": createdOut, "finished index out of range": finishedOut, "pr_units negative index": prOut,
		"created count mismatch": createdShort, "finished count mismatch": finishedShort, "pr_units count mismatch": prLong,
	} {
		if _, err := mapAnalyticsFlow(req, b, doc); err == nil {
			t.Errorf("%s: mapping succeeded, want an error (a 500, never a silent drop)", name)
		}
	}

	bad := flowDocFor(b)
	ts := "not-a-time"
	bad.WaitingNow.States = []store.AnalyticsFlowWaitState{{State: "queued", Count: 1, OldestStatusSince: &ts}}
	if _, err := mapAnalyticsFlow(req, b, bad); err == nil {
		t.Error("an unparsable timestamp must fail the mapping")
	}
}

func TestMapAnalyticsFlowFiltersEchoAndNoRepoIdentifiers(t *testing.T) {
	b := flowBounds(t, analytics.Window7d)
	repo := uuid.New()
	doc := flowDocFor(b)

	none, err := mapAnalyticsFlow(mustParseAnalytics(t, "", false, analytics.SectionFlow, uuid.New()), b, doc)
	if err != nil {
		t.Fatal(err)
	}
	if none.Filters.Repo != nil || none.Filters.Harness != nil || !slices.Equal(none.Filters.Kinds, runkind.All()) {
		t.Fatalf("unset filters = %+v, want nil/nil/every kind", none.Filters)
	}

	caller := uuid.New()
	dto, err := mapAnalyticsFlow(mustParseAnalytics(t, "repo="+repo.String()+"&harness=codex&kind=delivery", false, analytics.SectionFlow, caller), b, doc)
	if err != nil {
		t.Fatal(err)
	}
	if dto.Filters.Repo == nil || *dto.Filters.Repo != repo.String() || dto.Filters.Harness == nil || *dto.Filters.Harness != "codex" {
		t.Fatalf("filters = %+v", dto.Filters)
	}
	if !slices.Equal(dto.Filters.Kinds, runkind.Delivery()) {
		t.Fatalf("filters.kinds = %v, want the effective list %v", dto.Filters.Kinds, runkind.Delivery())
	}
	if dto.SelectedUser != caller.String() || dto.Scope.Route != "owner" {
		t.Fatalf("scope %+v selected_user %q", dto.Scope, dto.SelectedUser)
	}
	raw, _ := json.Marshal(dto)
	if n := strings.Count(string(raw), repo.String()); n != 1 {
		t.Fatalf("the repo selector appears %d times in the body, want exactly once (the filters echo): %s", n, raw)
	}
	if strings.Contains(string(raw), "repo_id") || strings.Contains(string(raw), "path_with_namespace") || strings.Contains(string(raw), "repo_path") {
		t.Fatalf("flow body names a repo identifier field: %s", raw)
	}
}
