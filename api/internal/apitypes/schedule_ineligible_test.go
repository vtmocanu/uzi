package apitypes

import (
	"encoding/json"
	"strings"
	"testing"
)

// Issue #1543: LastFire/RunNowResponse carry IneligibleMatched as a *int64 so an absent key
// (a historical fire persisted before the field existed, or a non-label sweep) decodes to
// nil = UNKNOWN, while an explicit 0 decodes to a non-nil known zero.

func TestLastFireIneligibleMatchedDecode(t *testing.T) {
	var legacy LastFire
	if err := json.Unmarshal([]byte(`{"fired_at":"2026-08-13T09:00:00Z","matched":0,"capped":false,"started":[],"skips":[]}`), &legacy); err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if legacy.IneligibleMatched != nil {
		t.Errorf("legacy last_fire without the key: IneligibleMatched = %d, want nil (unknown)", *legacy.IneligibleMatched)
	}

	var zero LastFire
	if err := json.Unmarshal([]byte(`{"fired_at":"2026-08-13T09:00:00Z","matched":0,"capped":false,"ineligible_matched":0,"started":[],"skips":[]}`), &zero); err != nil {
		t.Fatalf("decode zero: %v", err)
	}
	if zero.IneligibleMatched == nil || *zero.IneligibleMatched != 0 {
		t.Errorf(`"ineligible_matched":0 must decode to a non-nil 0, got %v`, zero.IneligibleMatched)
	}

	var set LastFire
	if err := json.Unmarshal([]byte(`{"ineligible_matched":16}`), &set); err != nil {
		t.Fatalf("decode set: %v", err)
	}
	if set.IneligibleMatched == nil || *set.IneligibleMatched != 16 {
		t.Errorf("ineligible_matched 16 decoded to %v", set.IneligibleMatched)
	}
}

func TestRunNowResponseIneligibleMatchedWire(t *testing.T) {
	var legacy RunNowResponse
	if err := json.Unmarshal([]byte(`{"created":0,"run_ids":[],"matched":0,"capped":false,"started":[],"skips":[]}`), &legacy); err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if legacy.IneligibleMatched != nil {
		t.Errorf("run-now without the key: IneligibleMatched = %d, want nil", *legacy.IneligibleMatched)
	}

	// Marshal: nil omits the key, a set 0 emits it.
	b, err := json.Marshal(RunNowResponse{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ineligible_matched") {
		t.Errorf("nil IneligibleMatched must omit the key: %s", b)
	}
	zero := int64(0)
	b, err = json.Marshal(RunNowResponse{IneligibleMatched: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ineligible_matched":0`) {
		t.Errorf("a known 0 must be emitted: %s", b)
	}
}
