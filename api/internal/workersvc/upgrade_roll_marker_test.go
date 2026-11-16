package workersvc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestNormalizeRollMarkerReport(t *testing.T) {
	long := strings.Repeat("x", 193)
	okBlocked := "waiting\x1b_for‮reg"
	for _, tc := range []struct {
		name string
		in   RollMarkerReport
		ok   bool
	}{
		{"valid", RollMarkerReport{State: "present", Canary: true, CoverageGroup: "0123456789abcdef", TargetVersion: "0.11.7", Blocked: &okBlocked}, true},
		{"absent", RollMarkerReport{State: "absent"}, true},
		{"unavailable", RollMarkerReport{State: "unavailable"}, true},
		{"bad state", RollMarkerReport{State: "bogus"}, false},
		{"empty state", RollMarkerReport{}, false},
		{"group uppercase", RollMarkerReport{State: "present", CoverageGroup: "0123456789ABCDEF"}, false},
		{"group short", RollMarkerReport{State: "present", CoverageGroup: "abc"}, false},
		{"version too long", RollMarkerReport{State: "present", TargetVersion: strings.Repeat("v", 129)}, false},
		{"version invalid utf8", RollMarkerReport{State: "present", TargetVersion: "\xff"}, false},
		{"blocked too long", RollMarkerReport{State: "present", Blocked: &long}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRollMarkerReport(&tc.in)
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v want ok=%v", err, tc.ok)
			}
			if err != nil && (strings.Contains(err.Error(), "bogus") || strings.Contains(err.Error(), "xxx")) {
				t.Fatalf("error carries controller value: %v", err)
			}
			if tc.name == "valid" {
				if got.Blocked == nil || *got.Blocked != "waiting_forreg" {
					t.Fatalf("unsafe runes not stripped: %q", *got.Blocked)
				}
			}
		})
	}
}

func TestParseRollMarkerReport(t *testing.T) {
	for _, raw := range []string{"", "null", "  null "} {
		if m, err := ParseRollMarkerReport(json.RawMessage(raw)); m != nil || err != nil {
			t.Errorf("%q -> %v, %v; want nil, nil", raw, m, err)
		}
	}
	if m, err := ParseRollMarkerReport(nil); m != nil || err != nil {
		t.Errorf("absent -> %v, %v", m, err)
	}
	for _, raw := range []string{`{"state":"present","canary":"yes"}`, `{"state":5}`, `[1]`, `"x"`, `{"state":"bogus"}`} {
		if m, err := ParseRollMarkerReport(json.RawMessage(raw)); err == nil || m != nil {
			t.Errorf("%s -> %v, %v; want error", raw, m, err)
		}
	}
	m, err := ParseRollMarkerReport(json.RawMessage(`{"state":"present","canary":true,"reservation":true}`))
	if err != nil || m == nil || m.State != "present" || !m.Canary || !m.Reservation {
		t.Fatalf("valid -> %+v, %v", m, err)
	}
}

func TestParseFreshRollMarker(t *testing.T) {
	now := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	data := []byte(`{"state":"present","completed":true,"reservation":true}`)
	at := func(d time.Duration) pgtype.Timestamptz { return pgtype.Timestamptz{Time: now.Add(d), Valid: true} }

	if m := ParseFreshRollMarker(data, at(-10*time.Second), now); m == nil || !m.Completed {
		t.Fatalf("fresh marker = %+v", m)
	}
	if m := ParseFreshRollMarker(data, at(-DefaultControllerSignalTTL-time.Second), now); m != nil {
		t.Fatalf("expired marker = %+v", m)
	}
	if m := ParseFreshRollMarker(data, at(time.Second), now); m != nil {
		t.Fatalf("future receipt = %+v", m)
	}
	if m := ParseFreshRollMarker(data, pgtype.Timestamptz{}, now); m != nil {
		t.Fatalf("invalid receipt = %+v", m)
	}
	if m := ParseFreshRollMarker(nil, at(0), now); m != nil {
		t.Fatalf("empty data = %+v", m)
	}
	big := []byte(`{"state":"present","target_version":"` + strings.Repeat("x", 1100) + `"}`)
	if m := ParseFreshRollMarker(big, at(0), now); m != nil {
		t.Fatalf("oversized = %+v", m)
	}
}
