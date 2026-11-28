package handler

import (
	"bytes"
	"context"
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
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4771: the stored all-time Spend request shape and serving path, with no database.

func parseAllTime(query string, admin bool, section string, on bool) (analyticsRequest, string) {
	q, err := url.ParseQuery(query)
	if err != nil {
		panic(err)
	}
	return parseAnalyticsRequest(q, admin, section, uuid.New(), on)
}

func TestAnalyticsAllTimePrecedence(t *testing.T) {
	repo := uuid.New().String()
	const unfiltered = "window=all"
	cases := []struct {
		name  string
		query string
		on    string // message with the setting on
		off   string // message with the setting off
	}{
		{"unfiltered", unfiltered, "", msgAnalyticsWindowAllAllUsers},
		{"user=all", unfiltered + "&user=all", "", msgAnalyticsWindowAllAllUsers},
		{"tz=UTC explicit", unfiltered + "&tz=UTC", "", msgAnalyticsWindowAllAllUsers},
		{"valid split and breakdown", unfiltered + "&split=user&breakdown=user", "", msgAnalyticsWindowAllAllUsers},
		{"repo", unfiltered + "&repo=" + repo, msgAnalyticsAllTimeUnfilteredUTC, msgAnalyticsWindowAllAllUsers},
		{"harness", unfiltered + "&harness=claude", msgAnalyticsAllTimeUnfilteredUTC, msgAnalyticsWindowAllAllUsers},
		{"kind delivery", unfiltered + "&kind=delivery", msgAnalyticsAllTimeUnfilteredUTC, msgAnalyticsWindowAllAllUsers},
		{"kind supporting", unfiltered + "&kind=supporting", msgAnalyticsAllTimeUnfilteredUTC, msgAnalyticsWindowAllAllUsers},
		{"kind single", unfiltered + "&kind=issue", msgAnalyticsAllTimeUnfilteredUTC, msgAnalyticsWindowAllAllUsers},
		{"tz Paris", unfiltered + "&tz=Europe/Paris", msgAnalyticsAllTimeUnfilteredUTC, msgAnalyticsWindowAllAllUsers},
		{"filtered, bad split", unfiltered + "&harness=claude&split=nope", msgAnalyticsAllTimeUnfilteredUTC, msgAnalyticsWindowAllAllUsers},
		{"unfiltered, bad split", unfiltered + "&split=nope", "invalid split", msgAnalyticsWindowAllAllUsers},
		{"tz Local", unfiltered + "&tz=Local", "invalid tz", "invalid tz"},
		{"malformed repo", unfiltered + "&repo=nope", "invalid repo", "invalid repo"},
		{"bad harness", unfiltered + "&harness=nope", "invalid harness", "invalid harness"},
		{"bad kind", unfiltered + "&kind=nope", "invalid kind", "invalid kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, on := range []bool{true, false} {
				want := c.off
				if on {
					want = c.on
				}
				req, msg := parseAllTime(c.query, true, analytics.SectionSpend, on)
				if msg != want {
					t.Fatalf("setting on=%v: message %q, want %q", on, msg, want)
				}
				if wantPre := on && c.on == ""; msg == "" && req.precomputed != wantPre {
					t.Fatalf("setting on=%v: precomputed = %v, want %v", on, req.precomputed, wantPre)
				}
			}
		})
	}
}

func TestAnalyticsAllTimeOnlyAdminAllUsersSpend(t *testing.T) {
	for _, section := range []string{analytics.SectionReliability, analytics.SectionCapacity, analytics.SectionQuality, analytics.SectionFlow} {
		for _, q := range []string{"window=all", "window=all&user=all"} {
			if _, msg := parseAllTime(q, true, section, true); msg != msgAnalyticsWindowAllAllUsers {
				t.Fatalf("%s %q with the setting on: %q, want %q", section, q, msg, msgAnalyticsWindowAllAllUsers)
			}
		}
	}
	// A single user's all-time view and the owner route stay live.
	for _, tc := range []struct {
		query string
		admin bool
	}{{"window=all&user=" + uuid.New().String(), true}, {"window=all", false}, {"window=all&harness=claude", false}} {
		req, msg := parseAllTime(tc.query, tc.admin, analytics.SectionSpend, true)
		if msg != "" || req.precomputed {
			t.Fatalf("%q admin=%v: msg=%q precomputed=%v, want live", tc.query, tc.admin, msg, req.precomputed)
		}
	}
	// Other windows never use the stored path.
	if req, msg := parseAllTime("window=30d", true, analytics.SectionSpend, true); msg != "" || req.precomputed {
		t.Fatalf("window=30d: msg=%q precomputed=%v", msg, req.precomputed)
	}
}

func TestAnalyticsOptionWindowsFollowSetting(t *testing.T) {
	uid := uuid.New().String()
	cases := []struct {
		name  string
		query string
		admin bool
		on    bool
		want  []string
	}{
		{"admin all-users on", "", true, true, []string{"7d", "30d", "90d", "all"}},
		{"admin user=all on", "user=all", true, true, []string{"7d", "30d", "90d", "all"}},
		{"admin all-users off", "", true, false, []string{"7d", "30d", "90d"}},
		{"admin user=uuid on", "user=" + uid, true, true, []string{"7d", "30d", "90d", "all"}},
		{"admin user=uuid off", "user=" + uid, true, false, []string{"7d", "30d", "90d", "all"}},
		{"owner on", "", false, true, []string{"7d", "30d", "90d", "all"}},
		{"owner off", "", false, false, []string{"7d", "30d", "90d", "all"}},
	}
	for _, c := range cases {
		req, msg := parseAllTime(c.query, c.admin, analytics.SectionOptions, c.on)
		if msg != "" {
			t.Fatalf("%s: %s", c.name, msg)
		}
		if got := req.optionWindows(); !slices.Equal(got, c.want) {
			t.Fatalf("%s: windows %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAllTimeSpendKeys(t *testing.T) {
	keys := AllTimeSpendKeys()
	if len(keys) != 25 {
		t.Fatalf("%d keys, want 25", len(keys))
	}
	if keys[0] != "split=usage_model&breakdown=primary_model" || keys[1] != "split=usage_model&breakdown=harness" ||
		keys[5] != "split=harness&breakdown=primary_model" || keys[24] != "split=user&breakdown=user" {
		t.Fatalf("unexpected order: first %q, [1] %q, [5] %q, last %q", keys[0], keys[1], keys[5], keys[24])
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			t.Fatalf("duplicate key %q", k)
		}
		seen[k] = true
	}
}

func TestAnalyticsSnapshotStale(t *testing.T) {
	gen := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		interval time.Duration
		elapsed  time.Duration
		want     bool
	}{
		{"fresh", 4 * time.Hour, time.Hour, false},
		{"exactly twice", 4 * time.Hour, 8 * time.Hour, false},
		{"twice plus one microsecond", 4 * time.Hour, 8*time.Hour + time.Microsecond, true},
		{"near-max interval not stale", 2562047 * time.Hour, 24 * time.Hour, false},
	} {
		if got := analyticsSnapshotStale(gen.Add(tc.elapsed), gen, tc.interval); got != tc.want {
			t.Fatalf("%s: stale = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Exactly twice a near-max interval (beyond what one Duration holds) is not stale; a microsecond
	// later is.
	big := 2562047 * time.Hour
	boundary := gen.Add(big).Add(big)
	if analyticsSnapshotStale(boundary, gen, big) || !analyticsSnapshotStale(boundary.Add(time.Microsecond), gen, big) {
		t.Fatal("near-max interval boundary is wrong")
	}
	// A near-max interval pushes the deadline far out: the computation must neither wrap nor panic.
	if analyticsSnapshotStale(gen.AddDate(500, 0, 0), gen, 2562047*time.Hour) {
		t.Fatal("500 years after generation with a near-max interval reported stale")
	}
}

// fakeDocs is a counting stand-in for the stored-document read.
type fakeDocs struct {
	reads int
	doc   []byte
	at    time.Time
	err   error
	keys  []string
}

func (f *fakeDocs) read(_ context.Context, section, key string) ([]byte, time.Time, error) {
	f.reads++
	f.keys = append(f.keys, section+"|"+key)
	return f.doc, f.at, f.err
}

func newPrecomputedHandler(t *testing.T, docs *fakeDocs, now time.Time) (*Handler, *countingBeginner) {
	t.Helper()
	begin := &countingBeginner{}
	h := newAnalyticsUnitHandler(t, begin)
	h.cfg.AnalyticsSnapshotInterval = 4 * time.Hour
	h.now = func() time.Time { return now }
	h.analyticsPrecomputedDoc = docs.read
	return h, begin
}

func serveAdminSpend(h *Handler, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.AdminAnalyticsSpend(rec, analyticsGet(store.User{ID: uuid.New(), IsAdmin: true}, "/api/admin/analytics/spend?"+query))
	return rec
}

func TestAnalyticsAllTimeServePending(t *testing.T) {
	docs := &fakeDocs{err: pgx.ErrNoRows}
	h, begin := newPrecomputedHandler(t, docs, time.Now())
	rec := serveAdminSpend(h, "window=all")
	if rec.Code != http.StatusServiceUnavailable || rec.Result().Header.Get("Retry-After") != "60" || rec.Result().Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("status %d Retry-After %q Cache-Control %q", rec.Code, rec.Result().Header.Get("Retry-After"), rec.Result().Header.Get("Cache-Control"))
	}
	if !strings.Contains(rec.Body.String(), "analytics snapshot pending, retry later") {
		t.Fatalf("body %s", rec.Body.String())
	}
	if begin.calls.Load() != 0 {
		t.Fatal("a stored read began a snapshot transaction")
	}
}

func TestAnalyticsAllTimeServeReadErrors(t *testing.T) {
	h, _ := newPrecomputedHandler(t, &fakeDocs{err: context.DeadlineExceeded}, time.Now())
	rec := serveAdminSpend(h, "window=all")
	if rec.Code != http.StatusServiceUnavailable || rec.Result().Header.Get("Retry-After") != "1" || rec.Result().Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("timeout: status %d Retry-After %q", rec.Code, rec.Result().Header.Get("Retry-After"))
	}
	h, _ = newPrecomputedHandler(t, &fakeDocs{err: errors.New("boom secret detail")}, time.Now())
	rec = serveAdminSpend(h, "window=all")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("other error: status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyticsAllTimeServeRejectsNonObjectDoc(t *testing.T) {
	for _, doc := range []string{"", "{}", " { } ", "[1]", "null", `"x"`, `{"a":1`, "{x}", `{"a":1,}`} {
		h, _ := newPrecomputedHandler(t, &fakeDocs{doc: []byte(doc), at: time.Now()}, time.Now())
		rec := serveAdminSpend(h, "window=all")
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "snapshot") {
			t.Fatalf("doc %q: status %d body %s", doc, rec.Code, rec.Body.String())
		}
	}
}

// The spliced bytes equal what httpx.JSON writes for the corresponding wrapper DTO, and the
// snapshot member reflects the interval at the handler's clock.
func TestAnalyticsAllTimeServeSplicedBytesEqualEncoder(t *testing.T) {
	gen := time.Date(2031, 3, 4, 5, 6, 7, 123456000, time.UTC)
	var spend apitypes.AnalyticsSpendDTO
	spend.SelectedUser = "all"
	spend.Scope = apitypes.AnalyticsScopeDTO{Route: "admin", User: "all"}
	spend.Split.Dimension = "repo<&>"
	stored, err := json.Marshal(spend)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		now   time.Time
		stale bool
	}{
		{"fresh", gen.Add(time.Hour), false},
		{"exactly twice", gen.Add(8 * time.Hour), false},
		{"past twice", gen.Add(8*time.Hour + time.Microsecond), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs := &fakeDocs{doc: stored, at: gen}
			h, begin := newPrecomputedHandler(t, docs, tc.now)
			rec := serveAdminSpend(h, "window=all&split=repo&breakdown=kind")
			if rec.Code != http.StatusOK || rec.Result().Header.Get("Cache-Control") != "private, no-store" || rec.Result().Header.Get("Content-Type") != "application/json" {
				t.Fatalf("status %d headers %v", rec.Code, rec.Result().Header)
			}
			var decoded apitypes.AnalyticsSpendDTO
			if err := json.Unmarshal(stored, &decoded); err != nil {
				t.Fatal(err)
			}
			want := httptest.NewRecorder()
			httpx.JSON(want, http.StatusOK, apitypes.AnalyticsSpendSnapshotDTO{
				AnalyticsSpendDTO: decoded,
				Snapshot:          apitypes.AnalyticsSnapshotMetaDTO{GeneratedAt: gen, Stale: tc.stale},
			})
			if !bytes.Equal(rec.Body.Bytes(), want.Body.Bytes()) {
				t.Fatalf("spliced body differs from the encoder's\n got: %.300s\nwant: %.300s", rec.Body.String(), want.Body.String())
			}
			if !slices.Equal(docs.keys, []string{"spend|split=repo&breakdown=kind"}) {
				t.Fatalf("read keys %v", docs.keys)
			}
			if begin.calls.Load() != 0 {
				t.Fatal("a stored read began a snapshot transaction")
			}
		})
	}
}

func TestAnalyticsAllTimeServeUserOmittedAndAllReadTheSameKey(t *testing.T) {
	stored := []byte(`{"selected_user":"all"}`)
	docs := &fakeDocs{doc: stored, at: time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)}
	h, _ := newPrecomputedHandler(t, docs, docs.at.Add(time.Hour))
	a := serveAdminSpend(h, "window=all")
	b := serveAdminSpend(h, "window=all&user=all")
	if a.Code != http.StatusOK || a.Body.String() != b.Body.String() {
		t.Fatalf("omitted %d %q vs user=all %d %q", a.Code, a.Body.String(), b.Code, b.Body.String())
	}
	if !slices.Equal(docs.keys, []string{"spend|split=usage_model&breakdown=repo", "spend|split=usage_model&breakdown=repo"}) {
		t.Fatalf("keys %v", docs.keys)
	}
}

func TestAnalyticsAllTimeFilteredNeverReadsStored(t *testing.T) {
	docs := &fakeDocs{doc: []byte(`{"a":1}`), at: time.Now()}
	h, _ := newPrecomputedHandler(t, docs, time.Now())
	rec := serveAdminSpend(h, "window=all&harness=claude")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), msgAnalyticsAllTimeUnfilteredUTC) || docs.reads != 0 {
		t.Fatalf("status %d body %s reads %d", rec.Code, rec.Body.String(), docs.reads)
	}
}
