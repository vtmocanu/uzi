package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4685: Reliability handler tests that need no database. A recording transaction stands
// in for the pool so a request that must read nothing is proven to run no statement, and the
// kinds the handler hands to the store are visible.

type recordedStatement struct {
	sql  string
	args []any
}

// recordingTx records every Query and QueryRow and fails the first one, so a request stops at
// its first statement. Exec (SET LOCAL) succeeds and is not recorded.
type recordingTx struct {
	pgx.Tx
	mu   sync.Mutex
	seen []recordedStatement
}

func (t *recordingTx) record(sql string, args []any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen = append(t.seen, recordedStatement{sql: sql, args: args})
}

func (t *recordingTx) statements() []recordedStatement {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.seen)
}

func (t *recordingTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (t *recordingTx) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	t.record(sql, args)
	return nil, errors.New("recording tx: stop")
}

func (t *recordingTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	t.record(sql, args)
	return stopRow{}
}

func (t *recordingTx) Rollback(context.Context) error { return nil }

type stopRow struct{}

func (stopRow) Scan(...any) error { return errors.New("recording tx: stop") }

type recordingBeginner struct{ tx *recordingTx }

func (b *recordingBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return b.tx, nil }

// newReliabilityUnitHandler wires a handler whose timezone set is preloaded with UTC, so the
// authorize step of a UTC request with no repo or user filter runs no statement.
func newReliabilityUnitHandler(t *testing.T, tx *recordingTx) *Handler {
	t.Helper()
	h := newAnalyticsUnitHandler(t, &recordingBeginner{tx: tx})
	h.analyticsDeps.tzSet = map[string]struct{}{"UTC": {}}
	return h
}

func decodeReliability(t *testing.T, rec *httptest.ResponseRecorder) (apitypes.AnalyticsReliabilityDTO, map[string]json.RawMessage) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var dto apitypes.AnalyticsReliabilityDTO
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dto); err != nil {
		t.Fatalf("decode reliability: %v: %s", err, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	return dto, raw
}

// A kind filter that selects no delivery kind answers applicable=false and reads nothing: the
// store cohort does not filter fn_outcome_kind itself, so a supporting kind must never reach it.
func TestAnalyticsReliabilitySupportingOnlyRunsNoStatement(t *testing.T) {
	for _, rt := range []analyticsRoute{analyticsRoutes[analyticsOwnerReliabilityRoute], analyticsRoutes[analyticsAdminReliabilityRoute]} {
		for _, kind := range []string{"supporting", "chat", "judge", "cross_check"} {
			for _, window := range []string{"30d", "all"} {
				if rt.admin && window == "all" {
					continue // admin all-users window=all is a 400 (msgAnalyticsWindowAllAllUsers)
				}
				t.Run(rt.name+" "+kind+" "+window, func(t *testing.T) {
					tx := &recordingTx{}
					h := newReliabilityUnitHandler(t, tx)
					rec := httptest.NewRecorder()
					h.analyticsServe(rt, rec, analyticsGet(store.User{ID: uuid.New(), IsAdmin: rt.admin}, rt.path+"?kind="+kind+"&window="+window))
					dto, raw := decodeReliability(t, rec)
					if got := tx.statements(); len(got) != 0 {
						t.Fatalf("ran %d statements for a supporting-only filter, want 0; first: %s", len(got), got[0].sql)
					}
					if dto.Applicable || dto.Reason == nil || *dto.Reason != "supporting_kinds_only" {
						t.Fatalf("applicable = %v reason %v, want false with supporting_kinds_only", dto.Applicable, dto.Reason)
					}
					if string(raw["latest_failure"]) != "null" {
						t.Fatalf("latest_failure = %s, want null", raw["latest_failure"])
					}
					cur := dto.Totals.Current
					if cur.Finished != 0 || cur.RequeuedRuns != 0 || cur.Outcomes != (apitypes.AnalyticsOutcomesDTO{}) {
						t.Fatalf("totals.current = %+v, want zero counts", cur)
					}
					if cur.FailureRate.Value != nil || cur.FailureRate.Reason == nil || *cur.FailureRate.Reason != "not_applicable" {
						t.Fatalf("failure_rate = %+v, want null with not_applicable", cur.FailureRate)
					}
					if (window == "all") != (dto.Totals.Previous == nil) {
						t.Fatalf("window %s: previous = %v", window, dto.Totals.Previous)
					}
					if len(dto.Series) != len(dto.Buckets) || len(dto.RollingFailureRate.Overall) != len(dto.Rolling) {
						t.Fatalf("series %d/%d buckets, rolling points %d/%d rolling: must align", len(dto.Series), len(dto.Buckets), len(dto.RollingFailureRate.Overall), len(dto.Rolling))
					}
					if window == "30d" && len(dto.Series) != 30 {
						t.Fatalf("30d series = %d, want 30", len(dto.Series))
					}
					for _, p := range dto.RollingFailureRate.Overall {
						if p.Value != nil || p.Reason == nil || *p.Reason != "not_applicable" || p.Finished != 0 || p.Failed != 0 {
							t.Fatalf("rolling point = %+v, want zero counts and null with not_applicable", p)
						}
					}
					if len(dto.RollingFailureRate.Harnesses) != 2 || dto.Causes.Failed != 0 || len(dto.Causes.Items) != 0 || len(dto.Breakdown.Rows) != 0 || dto.NeedsLanding.Runs != 0 {
						t.Fatalf("panels = %+v %+v %+v %+v", dto.RollingFailureRate.Harnesses, dto.Causes, dto.Breakdown, dto.NeedsLanding)
					}
					for _, k := range []string{"series", "buckets", "rolling"} {
						v, ok := raw[k]
						if !ok || !bytes.HasPrefix(bytes.TrimSpace(v), []byte("[")) {
							t.Fatalf("%s = %q (present %v), want a JSON array", k, v, ok)
						}
					}
					if dto.Breakdown.Dimension != "primary_model" {
						t.Fatalf("breakdown dimension = %q, want the default primary_model", dto.Breakdown.Dimension)
					}
				})
			}
		}
	}
}

// Only Delivery kinds reach the store, whatever the kind filter selects: an unfiltered request
// (every kind, chat included) and "delivery" hand the Delivery set to the first statement, and a
// single delivery kind passes through alone.
func TestAnalyticsReliabilityHandsOnlyDeliveryKindsToTheStore(t *testing.T) {
	delivery := runkind.Delivery()
	slices.Sort(delivery)
	cases := []struct {
		query string
		want  []string
	}{
		{"", delivery},
		{"kind=delivery", delivery},
		{"kind=issue", []string{runkind.Issue}},
		{"kind=job&harness=codex", []string{runkind.Job}},
	}
	for _, rt := range []analyticsRoute{analyticsRoutes[analyticsOwnerReliabilityRoute], analyticsRoutes[analyticsAdminReliabilityRoute]} {
		for _, tc := range cases {
			t.Run(rt.name+" ?"+tc.query, func(t *testing.T) {
				tx := &recordingTx{}
				h := newReliabilityUnitHandler(t, tx)
				rec := httptest.NewRecorder()
				h.analyticsServe(rt, rec, analyticsGet(store.User{ID: uuid.New(), IsAdmin: rt.admin}, rt.path+"?"+tc.query))
				got := tx.statements()
				if len(got) == 0 {
					t.Fatalf("no statement ran (status %d %s)", rec.Code, rec.Body.String())
				}
				if !strings.Contains(got[0].sql, "-- name: Analytics") || !strings.Contains(got[0].sql, "HistoryStart") {
					t.Fatalf("first statement = %.60s, want the history start", got[0].sql)
				}
				var kinds []string
				for _, a := range got[0].args {
					if ks, ok := a.([]string); ok {
						kinds = ks
					}
				}
				sorted := slices.Sorted(slices.Values(kinds))
				if !slices.Equal(sorted, tc.want) {
					t.Fatalf("kinds = %v, want %v", sorted, tc.want)
				}
				for _, k := range runkind.Supporting() {
					if slices.Contains(kinds, k) {
						t.Fatalf("supporting kind %q reached the store: %v", k, kinds)
					}
				}
			})
		}
	}
}

func TestAnalyticsReliabilityParseDefaults(t *testing.T) {
	caller := uuid.New()
	rel := mustParseAnalytics(t, "", false, analytics.SectionReliability, caller)
	if rel.breakdown != "primary_model" || rel.window != analytics.Window30d || !slices.Equal(rel.kinds, runkind.All()) {
		t.Fatalf("reliability defaults = breakdown %q window %q kinds %v", rel.breakdown, rel.window, rel.kinds)
	}
	if got := mustParseAnalytics(t, "", false, analytics.SectionSpend, caller).breakdown; got != "repo" {
		t.Fatalf("spend default breakdown = %q, want repo (unchanged)", got)
	}
	for _, b := range []string{"primary_model", "harness", "repo", "kind"} {
		if got := mustParseAnalytics(t, "breakdown="+b, false, analytics.SectionReliability, caller).breakdown; got != b {
			t.Fatalf("breakdown=%s parsed as %q", b, got)
		}
	}
	if got := mustParseAnalytics(t, "breakdown=user", true, analytics.SectionReliability, caller).breakdown; got != "user" {
		t.Fatalf("admin all-users breakdown=user parsed as %q", got)
	}
	// The window choices are Spend's: all-time is offered to every scope but admin all-users.
	if got := mustParseAnalytics(t, "window=all&user="+uuid.NewString(), true, analytics.SectionReliability, caller).window; got != analytics.WindowAll {
		t.Fatalf("admin user=<uuid> window=all parsed as %q", got)
	}
}

// The owner route never reveals repos and always scopes to the caller; admin user=<uuid> reveals
// for that user; admin-all uses the all-scope statement. RevealRepos derives from the route.
func TestAnalyticsReliabilityStatementScopeAndReveal(t *testing.T) {
	caller, selected := uuid.New(), uuid.New()
	bounds, err := analytics.ComputeBounds(time.Date(2031, 6, 15, 12, 0, 0, 0, time.UTC), time.UTC, analytics.Window30d, nil)
	if err != nil {
		t.Fatal(err)
	}
	kinds := runkind.Delivery()

	owner := buildOwnerReliabilityParams(mustParseAnalytics(t, "kind=issue", false, analytics.SectionReliability, caller), bounds, kinds)
	if owner.RevealRepos || owner.UserID != caller {
		t.Fatalf("owner route params: reveal %v user %s, want false and the caller %s", owner.RevealRepos, owner.UserID, caller)
	}
	byUser := buildOwnerReliabilityParams(mustParseAnalytics(t, "user="+selected.String(), true, analytics.SectionReliability, caller), bounds, kinds)
	if !byUser.RevealRepos || byUser.UserID != selected {
		t.Fatalf("admin user=<uuid> params: reveal %v user %s, want true and %s", byUser.RevealRepos, byUser.UserID, selected)
	}
	if !slices.Equal(owner.RollHarnesses, []string{"claude", "codex"}) {
		t.Fatalf("roll harnesses = %v", owner.RollHarnesses)
	}
	if !slices.Equal(owner.LandableOrigins, workersvc.AllHumanLandableFailOrigins()) || len(owner.LandableOrigins) == 0 {
		t.Fatalf("landable origins = %v", owner.LandableOrigins)
	}
	if owner.Breakdown != "primary_model" || !owner.FoldFrom.Valid || !owner.PreviousFrom.Valid {
		t.Fatalf("owner params = breakdown %q fold %v previous %v", owner.Breakdown, owner.FoldFrom, owner.PreviousFrom)
	}
	// The repo and harness cohort filters reach the statement arguments; absent filters stay NULL.
	repoID := uuid.New()
	filtered := "repo=" + repoID.String() + "&harness=codex"
	fo := buildOwnerReliabilityParams(mustParseAnalytics(t, filtered, false, analytics.SectionReliability, caller), bounds, kinds)
	if fo.RepoID != (pgtype.UUID{Bytes: repoID, Valid: true}) || fo.Harness != (pgtype.Text{String: "codex", Valid: true}) {
		t.Fatalf("owner params repo %v harness %v, want %s and codex", fo.RepoID, fo.Harness, repoID)
	}
	if owner.RepoID.Valid || owner.Harness.Valid {
		t.Fatalf("unfiltered owner params repo %v harness %v, want NULL", owner.RepoID, owner.Harness)
	}
	fa := buildAllReliabilityParams(mustParseAnalytics(t, filtered, true, analytics.SectionReliability, caller), bounds, kinds)
	if fa.RepoID != fo.RepoID || fa.Harness != fo.Harness {
		t.Fatalf("all-scope params repo %v harness %v, want %v and %v", fa.RepoID, fa.Harness, fo.RepoID, fo.Harness)
	}
	all := buildAllReliabilityParams(mustParseAnalytics(t, "breakdown=user", true, analytics.SectionReliability, caller), bounds, kinds)
	if all.RepoID.Valid || all.Harness.Valid {
		t.Fatalf("unfiltered all-scope params repo %v harness %v, want NULL", all.RepoID, all.Harness)
	}
	if all.Breakdown != "user" || !slices.Equal(all.Kinds, kinds) {
		t.Fatalf("all-scope params = %+v", all)
	}
	allTime, err := analytics.ComputeBounds(time.Date(2031, 6, 15, 12, 0, 0, 0, time.UTC), time.UTC, analytics.WindowAll, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := buildOwnerReliabilityParams(mustParseAnalytics(t, "window=all", false, analytics.SectionReliability, caller), allTime, kinds); p.PreviousFrom != (pgtype.Timestamptz{}) {
		t.Fatalf("window=all passes previous_from %v", p.PreviousFrom)
	}
}

func relDocFor(b analytics.Bounds) store.AnalyticsReliabilityDoc {
	doc := store.AnalyticsReliabilityDoc{Buckets: make([]store.AnalyticsReliabilityBucket, len(b.Buckets))}
	doc.Rolling.Overall = make([]store.AnalyticsReliabilityPoint, len(b.Rolling))
	for _, h := range []string{"claude", "codex"} {
		doc.Rolling.Harnesses = append(doc.Rolling.Harnesses, store.AnalyticsReliabilityHarnessSeries{Harness: h, Points: make([]store.AnalyticsReliabilityPoint, len(b.Rolling))})
	}
	for i := range doc.Buckets {
		doc.Buckets[i].Index = i
	}
	for i := range doc.Rolling.Overall {
		doc.Rolling.Overall[i].Index = i
	}
	return doc
}

func TestMapAnalyticsReliabilityShape(t *testing.T) {
	bounds, err := analytics.ComputeBounds(time.Date(2031, 6, 15, 12, 0, 0, 0, time.UTC), time.UTC, analytics.Window7d, nil)
	if err != nil {
		t.Fatal(err)
	}
	callerID := uuid.New()
	req := mustParseAnalytics(t, "window=7d", false, analytics.SectionReliability, callerID)
	doc := relDocFor(bounds)
	doc.NeedsLanding = 3

	dto, err := mapAnalyticsReliability(req, bounds, doc, store.AnalyticsOwnerLatestFailureRow{})
	if err != nil {
		t.Fatal(err)
	}
	if dto.SelectedUser != callerID.String() || !dto.Applicable || dto.Reason != nil {
		t.Fatalf("header = selected %q applicable %v reason %v", dto.SelectedUser, dto.Applicable, dto.Reason)
	}
	if dto.LatestFailure != nil {
		t.Fatalf("latest_failure = %+v for found=false, want nil (never a zero timestamp)", dto.LatestFailure)
	}
	if dto.Totals.Previous != nil {
		t.Fatalf("totals.previous = %+v, want nil when the document has no previous block", dto.Totals.Previous)
	}
	if dto.NeedsLanding.Runs != 3 || dto.NeedsLanding.Caveat != "work_may_already_have_landed" {
		t.Fatalf("needs_landing = %+v", dto.NeedsLanding)
	}
	if dto.Series == nil || dto.Causes.Items == nil || dto.RollingFailureRate.Overall == nil || dto.RollingFailureRate.Harnesses == nil || dto.Breakdown.Rows == nil || dto.Buckets == nil || dto.Rolling == nil {
		t.Fatal("a panel slice is nil, which would serialize as JSON null")
	}

	// A found failure maps to a UTC timestamp and the row's identifiers.
	failedAt := time.Date(2031, 6, 14, 9, 30, 0, 0, time.FixedZone("x", 3600))
	run, user := uuid.New(), uuid.New()
	dto, err = mapAnalyticsReliability(req, bounds, doc, store.AnalyticsOwnerLatestFailureRow{
		Found: true, FailedAt: pgtype.Timestamptz{Time: failedAt, Valid: true}, Origin: "agent_failure", RunID: run, FailedUserID: user, CompletedSinceLastFailure: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	lf := dto.LatestFailure
	if lf == nil || !lf.FailedAt.Equal(failedAt) || lf.FailedAt.Location() != time.UTC || lf.Origin != "agent_failure" ||
		lf.RunID != run.String() || lf.UserID != user.String() || lf.CompletedSinceLastFailure != 4 {
		t.Fatalf("latest_failure = %+v", lf)
	}

	// A document that does not match the calendar is an error, not a misaligned chart.
	short := relDocFor(bounds)
	short.Buckets = short.Buckets[:len(short.Buckets)-1]
	if _, err := mapAnalyticsReliability(req, bounds, short, store.AnalyticsOwnerLatestFailureRow{}); err == nil {
		t.Fatal("a document with a missing bucket must fail the mapping")
	}
	// A harness series that is short, misnamed or out of order is an error too.
	for name, mutate := range map[string]func(*store.AnalyticsReliabilityDoc){
		"short points": func(d *store.AnalyticsReliabilityDoc) {
			d.Rolling.Harnesses[1].Points = d.Rolling.Harnesses[1].Points[1:]
		},
		"missing series": func(d *store.AnalyticsReliabilityDoc) { d.Rolling.Harnesses = d.Rolling.Harnesses[:1] },
		"swapped order": func(d *store.AnalyticsReliabilityDoc) {
			d.Rolling.Harnesses[0], d.Rolling.Harnesses[1] = d.Rolling.Harnesses[1], d.Rolling.Harnesses[0]
		},
		"unknown harness": func(d *store.AnalyticsReliabilityDoc) { d.Rolling.Harnesses[1].Harness = "gemini" },
	} {
		bad := relDocFor(bounds)
		mutate(&bad)
		if _, err := mapAnalyticsReliability(req, bounds, bad, store.AnalyticsOwnerLatestFailureRow{}); err == nil {
			t.Fatalf("%s: a misaligned harness series must fail the mapping", name)
		}
	}
	// found without a timestamp is malformed.
	if _, err := mapAnalyticsReliability(req, bounds, doc, store.AnalyticsOwnerLatestFailureRow{Found: true}); err == nil {
		t.Fatal("found=true without failed_at must fail the mapping")
	}
}

// The latest-failure statements receive the same repo, harness and kinds filters as the cohort,
// and the owner statement the selected user.
func TestAnalyticsReliabilityLatestFailureParamsCarryFilters(t *testing.T) {
	caller, repoID := uuid.New(), uuid.New()
	kinds := runkind.Delivery()
	wantRepo, wantHarness := pgtype.UUID{Bytes: repoID, Valid: true}, pgtype.Text{String: "codex", Valid: true}
	q := "repo=" + repoID.String() + "&harness=codex"

	o := ownerLatestFailureParams(mustParseAnalytics(t, q, false, analytics.SectionReliability, caller), kinds)
	if o.UserID != caller || o.RepoID != wantRepo || o.Harness != wantHarness || !slices.Equal(o.Kinds, kinds) {
		t.Fatalf("owner latest params = %+v", o)
	}
	a := allLatestFailureParams(mustParseAnalytics(t, q, true, analytics.SectionReliability, caller), kinds)
	if a.RepoID != wantRepo || a.Harness != wantHarness || !slices.Equal(a.Kinds, kinds) {
		t.Fatalf("all latest params = %+v", a)
	}
	if u := ownerLatestFailureParams(mustParseAnalytics(t, "", false, analytics.SectionReliability, caller), kinds); u.RepoID.Valid || u.Harness.Valid {
		t.Fatalf("unfiltered owner latest params = %+v, want NULL filters", u)
	}
}
