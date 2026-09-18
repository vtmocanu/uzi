package apitypes

import (
	"encoding/json"
	"strings"
	"testing"
)

// PRD #1247 M6 (blocker 2): the request-presence / tri-state contract of
// OptionalCredentialOverride on ScheduleRequest. An OMITTED key, an explicit null, and an
// explicit value must all decode to DISTINCT states, and an omitted value must be dropped
// from the marshaled body (the `omitzero` + IsZero behaviour the CLI relies on to seed-and-keep).

func TestScheduleRequestCredentialOverridePresence(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantPresent bool
		wantNil     bool    // Value == nil (explicit null / omitted)
		wantMode    string  // when Value != nil
		wantSecret  *string // when Value != nil
	}{
		{name: "omitted", body: `{"target":"prompt"}`, wantPresent: false, wantNil: true},
		{name: "explicit null", body: `{"credential_override":null}`, wantPresent: true, wantNil: true},
		{name: "explicit inherit", body: `{"credential_override":{"mode":"inherit"}}`, wantPresent: true, wantMode: "inherit"},
		{name: "explicit auto", body: `{"credential_override":{"mode":"auto"}}`, wantPresent: true, wantMode: "auto"},
		{name: "explicit pinned", body: `{"credential_override":{"mode":"pinned","secret_id":"abc"}}`, wantPresent: true, wantMode: "pinned", wantSecret: strp("abc")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req ScheduleRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.body, err)
			}
			co := req.CredentialOverride
			if co.Present != tc.wantPresent {
				t.Fatalf("Present = %v, want %v", co.Present, tc.wantPresent)
			}
			if tc.wantNil {
				if co.Value != nil {
					t.Fatalf("Value = %+v, want nil", co.Value)
				}
				return
			}
			if co.Value == nil {
				t.Fatalf("Value = nil, want mode %q", tc.wantMode)
			}
			if co.Value.Mode != tc.wantMode {
				t.Fatalf("Value.Mode = %q, want %q", co.Value.Mode, tc.wantMode)
			}
			switch {
			case tc.wantSecret == nil && co.Value.SecretID != nil:
				t.Fatalf("Value.SecretID = %v, want nil", *co.Value.SecretID)
			case tc.wantSecret != nil && (co.Value.SecretID == nil || *co.Value.SecretID != *tc.wantSecret):
				t.Fatalf("Value.SecretID = %v, want %q", co.Value.SecretID, *tc.wantSecret)
			}
		})
	}
}

// TestScheduleRequestCredentialOverrideOmitzero: an omitted (Present=false) wrapper must not
// appear in the marshaled body (`omitzero`), while a present one round-trips.
func TestScheduleRequestCredentialOverrideOmitzero(t *testing.T) {
	// Omitted → absent key.
	b, err := json.Marshal(ScheduleRequest{Target: "prompt"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "credential_override") {
		t.Fatalf("an omitted (Present=false) override must be dropped from the body, got %s", b)
	}
	// Present value → key with the inner value.
	req := ScheduleRequest{Target: "prompt", CredentialOverride: OptionalCredentialOverride{Present: true, Value: &CredentialOverrideRequest{Mode: "auto"}}}
	b, err = json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal present: %v", err)
	}
	if !strings.Contains(string(b), `"credential_override":{"mode":"auto"}`) {
		t.Fatalf("a present override must marshal its inner value, got %s", b)
	}
	// Present with nil value → explicit null (a clear).
	req = ScheduleRequest{Target: "prompt", CredentialOverride: OptionalCredentialOverride{Present: true}}
	b, err = json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal present-null: %v", err)
	}
	if !strings.Contains(string(b), `"credential_override":null`) {
		t.Fatalf("a present nil-value override must marshal as null, got %s", b)
	}
}

func strp(s string) *string { return &s }
