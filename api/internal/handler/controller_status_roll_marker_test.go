package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func postStatus(t *testing.T, st *statusStore, body string) {
	t.Helper()
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(body)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

const markerWorkerID = "11111111-1111-1111-1111-111111111111"

func TestControllerStatusRollMarkerIngest(t *testing.T) {
	pacing := `{"state":"held","target_image":"repo:v2","target_version":"2"}`
	for _, tc := range []struct {
		name, marker string
		wantMarker   bool
	}{
		{"valid", `,"roll_marker":{"state":"present","canary":true,"completed":true,"coverage_group":"0123456789abcdef","reservation":true}`, true},
		{"wrong json types", `,"roll_marker":{"state":"present","canary":"yes"}`, false},
		{"bogus state", `,"roll_marker":{"state":"bogus"}`, false},
		{"not an object", `,"roll_marker":"x"`, false},
		{"null", `,"roll_marker":null`, false},
		{"older payload", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &statusStore{rows: 1}
			postStatus(t, st, `{"kind":"fleet","workers":[{"id":"`+markerWorkerID+`","phase":"settled","pacing":`+pacing+tc.marker+`}]}`)
			if len(st.upserts) != 1 {
				t.Fatalf("entry not accepted: %d upserts", len(st.upserts))
			}
			p := st.upserts[0]
			if (p.RollMarker != nil) != tc.wantMarker {
				t.Fatalf("RollMarker = %s, want present=%v", p.RollMarker, tc.wantMarker)
			}
			if string(p.RollMarker) == "null" {
				t.Fatal("JSON null sent instead of SQL NULL")
			}
			if !p.UpdateRollMarker {
				t.Fatal("fleet report must be authoritative for the marker")
			}
			if len(p.Pacing) == 0 || !p.UpdatePacing {
				t.Fatalf("pacing lost: %s", p.Pacing)
			}
			if tc.wantMarker {
				var m map[string]any
				if err := json.Unmarshal(p.RollMarker, &m); err != nil || m["coverage_group"] != "0123456789abcdef" {
					t.Fatalf("marker payload: %s %v", p.RollMarker, err)
				}
			}
		})
	}
}

func TestControllerStatusRollMarkerAuthority(t *testing.T) {
	marker := `,"roll_marker":{"state":"absent","reservation":false}`
	for _, tc := range []struct {
		kind, marker string
		update       bool
	}{
		{"fleet", ``, true},
		{"readiness", ``, false},
		{"readiness", marker, false},
		{"", ``, false},
		{"", marker, true},
	} {
		st := &statusStore{rows: 1}
		postStatus(t, st, `{"kind":"`+tc.kind+`","workers":[{"id":"`+markerWorkerID+`","phase":"settled"`+tc.marker+`}]}`)
		if len(st.upserts) != 1 || st.upserts[0].UpdateRollMarker != tc.update {
			t.Fatalf("kind=%q marker=%v: %+v", tc.kind, tc.marker != "", st.upserts)
		}
	}
}

func TestControllerStatusBadPacingStillDropsEntryWithMarker(t *testing.T) {
	st := &statusStore{rows: 1}
	postStatus(t, st, `{"kind":"fleet","workers":[{"id":"`+markerWorkerID+`","phase":"settled","pacing":{"state":"completed"},"roll_marker":{"state":"present"}}]}`)
	if len(st.upserts) != 0 {
		t.Fatal("bad pacing must still drop the entry")
	}
}

func TestRollMarkerDTO(t *testing.T) {
	now := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) pgtype.Timestamptz { return pgtype.Timestamptz{Time: now.Add(d), Valid: true} }
	present := []byte(`{"state":"present","canary":true,"completed":true,"target_version":"0.11.7","coverage_group":"0123456789abcdef","reservation":true,"blocked":"waiting"}`)

	d := rollMarkerDTO("hosted", present, ts(-5*time.Second), now)
	if d == nil || d.State != "present" || d.Canary == nil || !*d.Canary || d.Completed == nil || !*d.Completed ||
		d.CoverageGroup == nil || *d.CoverageGroup != "0123456789abcdef" || d.TargetVersion == nil || *d.TargetVersion != "0.11.7" ||
		d.Reservation == nil || !*d.Reservation || d.Blocked == nil || *d.Blocked != "waiting" || d.ObservedAt == nil {
		t.Fatalf("fresh present: %+v", d)
	}

	stale := rollMarkerDTO("hosted", present, ts(-time.Hour), now)
	if stale == nil || stale.State != "unknown" || stale.Canary != nil || stale.ObservedAt == nil || !stale.ObservedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("stale: %+v", stale)
	}
	null := rollMarkerDTO("hosted", nil, pgtype.Timestamptz{}, now)
	if null == nil || null.State != "unknown" || null.ObservedAt != nil {
		t.Fatalf("NULL: %+v", null)
	}
	if rollMarkerDTO("external", present, ts(0), now) != nil {
		t.Fatal("external worker got a marker")
	}
	un := rollMarkerDTO("hosted", []byte(`{"state":"unavailable","reservation":false}`), ts(0), now)
	ab := rollMarkerDTO("hosted", []byte(`{"state":"absent","reservation":false}`), ts(0), now)
	if un.State != "unavailable" || ab.State != "absent" || un.Canary != nil || ab.CoverageGroup != nil {
		t.Fatalf("unavailable=%+v absent=%+v", un, ab)
	}
}
