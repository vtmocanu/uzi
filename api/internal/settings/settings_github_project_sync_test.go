package settings

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #364 M2. The GitHub Projects v2 Status sync kill-switch mirrors JudgeEnabled:
// default OFF, strict "true"/"false", junk → default (never silently ON). Off by
// default so the whole sync feature is a no-op on a fresh instance and on upgrade.
func TestGithubProjectSyncEnabledAccessor(t *testing.T) {
	// Empty table → default OFF.
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.GithubProjectSyncEnabled(context.Background()); err != nil || got != false {
		t.Fatalf("GithubProjectSyncEnabled default = %v, %v; want false", got, err)
	}
	if DefaultGithubProjectSyncEnabled != "false" {
		t.Fatalf("DefaultGithubProjectSyncEnabled = %q, want \"false\"", DefaultGithubProjectSyncEnabled)
	}

	for _, tc := range []struct {
		stored string
		want   bool
	}{
		{"true", true},
		{"false", false},
		{"", false},       // empty → default false
		{"banana", false}, // junk → default OFF, never silently on
		{"TRUE", false},   // non-canonical → default, not a lenient parse
		{"1", false},
	} {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeyGithubProjectSyncEnabled, tc.stored)}}, time.Minute)
		if got, _ := c.GithubProjectSyncEnabled(context.Background()); got != tc.want {
			t.Errorf("GithubProjectSyncEnabled(stored=%q) = %v, want %v", tc.stored, got, tc.want)
		}
	}
}

// The key is Known (admin-writable) and routes through the strict bool validator.
func TestGithubProjectSyncEnabledKnownAndValidated(t *testing.T) {
	if !Known(KeyGithubProjectSyncEnabled) {
		t.Errorf("Known(%s) = false, want true", KeyGithubProjectSyncEnabled)
	}
	if IsSecret(KeyGithubProjectSyncEnabled) {
		t.Errorf("IsSecret(%s) = true, want false", KeyGithubProjectSyncEnabled)
	}
	for _, ok := range []string{"true", "false"} {
		if err := Validate(KeyGithubProjectSyncEnabled, ok); err != nil {
			t.Errorf("Validate(%s, %q) = %v, want nil", KeyGithubProjectSyncEnabled, ok, err)
		}
	}
	if err := Validate(KeyGithubProjectSyncEnabled, "banana"); err == nil {
		t.Errorf("Validate(%s, banana) = nil, want a bool rejection", KeyGithubProjectSyncEnabled)
	}
}
