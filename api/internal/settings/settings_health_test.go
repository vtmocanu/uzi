package settings

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestValidateHealthSeconds(t *testing.T) {
	ok := []string{"0", "60", "300", "2700", "86400", " 120 "}
	for _, v := range ok {
		if err := Validate(KeyHealthStallSeconds, v); err != nil {
			t.Errorf("Validate(health_stall_seconds, %q) = %v, want nil", v, err)
		}
	}
	// {0} ∪ [60, 86400]: reject negatives, 1–59, > day, and non-integers.
	bad := []string{"-1", "1", "59", "86401", "300.5", "", "abc", "1e3"}
	for _, v := range bad {
		if err := Validate(KeyHealthQueuedSeconds, v); err == nil {
			t.Errorf("Validate(health_queued_seconds, %q) = nil, want a rejection", v)
		}
	}
}

func TestValidateHealthPercent(t *testing.T) {
	// {0} ∪ [50, 99]: 0 disables, the floor rejects noisy low thresholds, and 100 is
	// excluded because the sweeper fires at 100%.
	ok := []string{"0", "50", "85", "99", " 85 "}
	for _, v := range ok {
		if err := Validate(KeyHealthNearTimeoutPct, v); err != nil {
			t.Errorf("Validate(health_near_timeout_pct, %q) = %v, want nil", v, err)
		}
	}
	bad := []string{"-1", "49", "100", "abc", "85.5", "", "1e2"}
	for _, v := range bad {
		if err := Validate(KeyHealthNearTimeoutPct, v); err == nil {
			t.Errorf("Validate(health_near_timeout_pct, %q) = nil, want a rejection", v)
		}
	}
	// The out-of-range message is exact (the admin card mirrors it).
	if err := Validate(KeyHealthNearTimeoutPct, "49"); err == nil ||
		err.Error() != "must be 0 (disabled) or between 50 and 99 percent" {
		t.Errorf("Validate(health_near_timeout_pct, 49) = %v, want the exact percent message", err)
	}
}

func TestValidateHealthEnabledIsBool(t *testing.T) {
	if err := Validate(KeyHealthEnabled, "true"); err != nil {
		t.Errorf("Validate(health_enabled, true) = %v, want nil", err)
	}
	if err := Validate(KeyHealthEnabled, "false"); err != nil {
		t.Errorf("Validate(health_enabled, false) = %v, want nil", err)
	}
	if err := Validate(KeyHealthEnabled, "1"); err == nil {
		t.Error("Validate(health_enabled, 1) = nil, want a bool rejection")
	}
}

func TestHealthAccessorsFallBackToDefaults(t *testing.T) {
	c := New(&fakeStore{}, time.Minute)
	ctx := context.Background()

	if got, _ := c.HealthEnabled(ctx); got != true {
		t.Errorf("HealthEnabled default = %v, want true", got)
	}
	for _, tc := range []struct {
		name string
		read func(context.Context) (int, error)
		want int
	}{
		{"stall", c.HealthStallSeconds, 300},
		{"near-timeout", c.HealthNearTimeoutPct, 85},
		{"queued", c.HealthQueuedSeconds, 600},
		{"approval", c.HealthApprovalSeconds, 3600},
		{"cooldown", c.HealthNudgeCooldownSeconds, 1800},
	} {
		if got, _ := tc.read(ctx); got != tc.want {
			t.Errorf("%s default = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestHealthAccessorsReadStoredRows(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{
		row(KeyHealthEnabled, "false"),
		row(KeyHealthStallSeconds, "120"),
		row(KeyHealthNearTimeoutPct, "0"), // disabled
	}}, time.Minute)
	ctx := context.Background()

	if got, _ := c.HealthEnabled(ctx); got != false {
		t.Errorf("HealthEnabled = %v, want false", got)
	}
	if got, _ := c.HealthStallSeconds(ctx); got != 120 {
		t.Errorf("HealthStallSeconds = %d, want 120", got)
	}
	if got, _ := c.HealthNearTimeoutPct(ctx); got != 0 {
		t.Errorf("HealthNearTimeoutPct = %d, want 0 (disabled)", got)
	}
}

func TestHealthEnabledJunkDefaultsOn(t *testing.T) {
	// A malformed value must never silently disable detection (defaults ON).
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyHealthEnabled, "banana")}}, time.Minute)
	if got, _ := c.HealthEnabled(context.Background()); got != true {
		t.Errorf("HealthEnabled(banana) = %v, want true (junk-tolerant default-on)", got)
	}
}

func TestHealthKeysKnownAndInDefaults(t *testing.T) {
	for _, k := range []string{
		KeyHealthEnabled, KeyHealthStallSeconds, KeyHealthNearTimeoutPct,
		KeyHealthQueuedSeconds, KeyHealthApprovalSeconds, KeyHealthNudgeCooldownSeconds,
	} {
		if !Known(k) {
			t.Errorf("Known(%q) = false, want true", k)
		}
		if _, ok := Defaults[k]; !ok {
			t.Errorf("Defaults[%q] missing — All/AdminView would not surface it", k)
		}
	}
}
