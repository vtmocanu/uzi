package settings

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The completion-interlock rollout switch (PRD #1226 M1, D1) mirrors the
// capability-aware kill-switch in shape — a bool, junk-tolerant, validated as a bool,
// present in Defaults so All/AdminView surface it — but defaults OFF (the deliberate
// opposite): a new run is interlocked only on an affirmative "true".

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

func TestCompletionInterlockRolloutFallsBackToDefaultOff(t *testing.T) {
	c := New(&fakeStore{}, time.Minute)
	if got, _ := c.CompletionInterlockRollout(context.Background()); got != false {
		t.Errorf("CompletionInterlockRollout default = %v, want false", got)
	}
}

func TestCompletionInterlockRolloutReadsStoredRow(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyCompletionInterlockRollout, "true")}}, time.Minute)
	if got, _ := c.CompletionInterlockRollout(context.Background()); got != true {
		t.Errorf("CompletionInterlockRollout = %v, want true", got)
	}
}

func TestCompletionInterlockRolloutJunkDefaultsOff(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyCompletionInterlockRollout, "banana")}}, time.Minute)
	if got, _ := c.CompletionInterlockRollout(context.Background()); got != false {
		t.Errorf("CompletionInterlockRollout(banana) = %v, want false (junk-tolerant default-off)", got)
	}
}

func TestCompletionInterlockRolloutKeyKnownAndInDefaults(t *testing.T) {
	if !Known(KeyCompletionInterlockRollout) {
		t.Errorf("Known(%q) = false, want true", KeyCompletionInterlockRollout)
	}
	if got, ok := Defaults[KeyCompletionInterlockRollout]; !ok {
		t.Errorf("Defaults[%q] missing — All/AdminView would not surface it", KeyCompletionInterlockRollout)
	} else if got != "false" {
		t.Errorf("Defaults[%q] = %q, want \"false\"", KeyCompletionInterlockRollout, got)
	}
}
