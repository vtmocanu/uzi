package settings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The completion-interlock switch (PRD #1226 M1, D1; #1626) mirrors the capability-aware
// kill-switch: a bool, junk-tolerant, validated as a bool, present in Defaults so
// All/AdminView surface it, and default ON. Only an explicit "false" keeps it off.

func TestValidateCompletionInterlockRolloutIsBool(t *testing.T) {
	if err := Validate(KeyCompletionInterlockRollout, "true"); err != nil {
		t.Errorf("Validate(completion_interlock_rollout, true) = %v, want nil", err)
	}
	if err := Validate(KeyCompletionInterlockRollout, "false"); err != nil {
		t.Errorf("Validate(completion_interlock_rollout, false) = %v, want nil", err)
	}
	if err := Validate(KeyCompletionInterlockRollout, "1"); err == nil {
		t.Error("Validate(completion_interlock_rollout, 1) = nil, want a bool rejection")
	}
}

func TestCompletionInterlockRolloutFallsBackToDefaultOn(t *testing.T) {
	c := New(&fakeStore{}, time.Minute)
	got, err := c.CompletionInterlockRollout(context.Background())
	if err != nil || got != true {
		t.Errorf("CompletionInterlockRollout default = %v, %v; want true, nil", got, err)
	}
}

func TestCompletionInterlockRolloutReadsStoredRow(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyCompletionInterlockRollout, "true")}}, time.Minute)
	if got, _ := c.CompletionInterlockRollout(context.Background()); got != true {
		t.Errorf("CompletionInterlockRollout = %v, want true", got)
	}
}

func TestCompletionInterlockRolloutExplicitFalseKeepsItOff(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyCompletionInterlockRollout, "false")}}, time.Minute)
	got, err := c.CompletionInterlockRollout(context.Background())
	if err != nil || got != false {
		t.Errorf("CompletionInterlockRollout(false row) = %v, %v; want false, nil (admin kill-switch)", got, err)
	}
}

func TestCompletionInterlockRolloutJunkDefaultsOn(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyCompletionInterlockRollout, "banana")}}, time.Minute)
	if got, _ := c.CompletionInterlockRollout(context.Background()); got != true {
		t.Errorf("CompletionInterlockRollout(banana) = %v, want true (junk falls to the default, on)", got)
	}
}

// A cold read error (no valid snapshot) returns the default (true) WITH the error; the
// workersvc caller discards any errored value, so no run is stamped on a cold failure.
func TestCompletionInterlockRolloutColdErrorReturnsDefaultAndError(t *testing.T) {
	c := New(&fakeStore{err: errors.New("db down")}, time.Minute)
	got, err := c.CompletionInterlockRollout(context.Background())
	if err == nil {
		t.Fatal("cold read error must propagate so the caller can refuse to stamp")
	}
	if got != true {
		t.Errorf("cold error value = %v, want the default true alongside the error", got)
	}
}

// A failed refresh with a valid cached snapshot keeps serving the cached value, error-free.
func TestCompletionInterlockRolloutStaleOnRefreshErrorKeepsCachedValue(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{row(KeyCompletionInterlockRollout, "false")}}
	now := time.Unix(0, 0)
	c := New(fs, time.Minute)
	c.now = func() time.Time { return now }
	if got, err := c.CompletionInterlockRollout(context.Background()); err != nil || got != false {
		t.Fatalf("prime = %v, %v; want false, nil", got, err)
	}
	now = now.Add(2 * time.Minute)
	fs.err = errors.New("db down")
	got, err := c.CompletionInterlockRollout(context.Background())
	if err != nil || got != false {
		t.Errorf("stale-on-error = %v, %v; want the cached false, nil", got, err)
	}
	if fs.calls.Load() != 2 {
		t.Errorf("store calls = %d, want 2 (the refresh was attempted)", fs.calls.Load())
	}
}

func TestCompletionInterlockRolloutKeyKnownAndInDefaults(t *testing.T) {
	if !Known(KeyCompletionInterlockRollout) {
		t.Errorf("Known(%q) = false, want true", KeyCompletionInterlockRollout)
	}
	if got, ok := Defaults[KeyCompletionInterlockRollout]; !ok {
		t.Errorf("Defaults[%q] missing — All/AdminView would not surface it", KeyCompletionInterlockRollout)
	} else if got != "true" {
		t.Errorf("Defaults[%q] = %q, want \"true\"", KeyCompletionInterlockRollout, got)
	}
}
