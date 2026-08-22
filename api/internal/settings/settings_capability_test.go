package settings

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The capability-aware scheduling kill-switch (PRD #84 Decision 13) mirrors
// health_enabled: a bool, default-on, junk-tolerant, validated as a bool, and
// present in Defaults so All/AdminView surface it.

func TestValidateCapabilityAwareSchedulingIsBool(t *testing.T) {
	if err := Validate(KeyCapabilityAwareScheduling, "true"); err != nil {
		t.Errorf("Validate(capability_aware_scheduling, true) = %v, want nil", err)
	}
	if err := Validate(KeyCapabilityAwareScheduling, "false"); err != nil {
		t.Errorf("Validate(capability_aware_scheduling, false) = %v, want nil", err)
	}
	if err := Validate(KeyCapabilityAwareScheduling, "1"); err == nil {
		t.Error("Validate(capability_aware_scheduling, 1) = nil, want a bool rejection")
	}
}

func TestCapabilityAwareSchedulingFallsBackToDefaultOn(t *testing.T) {
	c := New(&fakeStore{}, time.Minute)
	if got, _ := c.CapabilityAwareScheduling(context.Background()); got != true {
		t.Errorf("CapabilityAwareScheduling default = %v, want true", got)
	}
}

func TestCapabilityAwareSchedulingReadsStoredRow(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyCapabilityAwareScheduling, "false")}}, time.Minute)
	if got, _ := c.CapabilityAwareScheduling(context.Background()); got != false {
		t.Errorf("CapabilityAwareScheduling = %v, want false", got)
	}
}

func TestCapabilityAwareSchedulingJunkDefaultsOn(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyCapabilityAwareScheduling, "banana")}}, time.Minute)
	if got, _ := c.CapabilityAwareScheduling(context.Background()); got != true {
		t.Errorf("CapabilityAwareScheduling(banana) = %v, want true (junk-tolerant default-on)", got)
	}
}

func TestCapabilityAwareSchedulingKeyKnownAndInDefaults(t *testing.T) {
	if !Known(KeyCapabilityAwareScheduling) {
		t.Errorf("Known(%q) = false, want true", KeyCapabilityAwareScheduling)
	}
	if _, ok := Defaults[KeyCapabilityAwareScheduling]; !ok {
		t.Errorf("Defaults[%q] missing — All/AdminView would not surface it", KeyCapabilityAwareScheduling)
	}
}
