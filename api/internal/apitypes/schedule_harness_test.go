package apitypes

import (
	"encoding/json"
	"strings"
	"testing"
)

// PRD #1429 M4a review (test gap): OptionalHarness has no dedicated unit test, unlike its
// twin OptionalCredentialOverride (schedule_credential_test.go). This mirrors that file
// exactly: the request-presence / tri-state decode contract of OptionalHarness on
// ScheduleRequest, plus the `omitzero`-drops-the-key marshal contract. An OMITTED key, an
// explicit null, and an explicit value must all decode to DISTINCT states, and an omitted
// value must be dropped from the marshaled body (the `omitzero` + IsZero behaviour the CLI
// relies on to seed-and-keep the stored pin).

func TestScheduleRequestHarnessPresence(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantPresent bool
		wantNil     bool // Value == nil (explicit null / omitted)
		wantValue   string
	}{
		{name: "omitted", body: `{"target":"prompt"}`, wantPresent: false, wantNil: true},
		{name: "explicit null", body: `{"harness":null}`, wantPresent: true, wantNil: true},
		{name: "explicit claude", body: `{"harness":"claude"}`, wantPresent: true, wantValue: "claude"},
		{name: "explicit codex", body: `{"harness":"codex"}`, wantPresent: true, wantValue: "codex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req ScheduleRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.body, err)
			}
			h := req.Harness
			if h.Present != tc.wantPresent {
				t.Fatalf("Present = %v, want %v", h.Present, tc.wantPresent)
			}
			if tc.wantNil {
				if h.Value != nil {
					t.Fatalf("Value = %+v, want nil", h.Value)
				}
				return
			}
			if h.Value == nil {
				t.Fatalf("Value = nil, want %q", tc.wantValue)
			}
			if *h.Value != tc.wantValue {
				t.Fatalf("Value = %q, want %q", *h.Value, tc.wantValue)
			}
		})
	}
}

// TestScheduleRequestHarnessOmitzero: an omitted (Present=false) wrapper must not appear in
// the marshaled body (`omitzero`), while a present one (value or explicit null) round-trips.
func TestScheduleRequestHarnessOmitzero(t *testing.T) {
	// Omitted → absent key.
	b, err := json.Marshal(ScheduleRequest{Target: "prompt"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "harness") {
		t.Fatalf("an omitted (Present=false) harness must be dropped from the body, got %s", b)
	}
	// Present value → key with the inner value.
	codex := "codex"
	req := ScheduleRequest{Target: "prompt", Harness: OptionalHarness{Present: true, Value: &codex}}
	b, err = json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal present: %v", err)
	}
	if !strings.Contains(string(b), `"harness":"codex"`) {
		t.Fatalf("a present harness must marshal its inner value, got %s", b)
	}
	// Present with nil value → explicit null (a clear back to implicit D11).
	req = ScheduleRequest{Target: "prompt", Harness: OptionalHarness{Present: true}}
	b, err = json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal present-null: %v", err)
	}
	if !strings.Contains(string(b), `"harness":null`) {
		t.Fatalf("a present nil-value harness must marshal as null, got %s", b)
	}
}
