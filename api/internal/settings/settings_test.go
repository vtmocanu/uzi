package settings

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeStore is an in-memory Store: it returns a fixed row set and counts calls
// so the cache's refresh behavior is observable, and can be flipped to error.
type fakeStore struct {
	rows  []store.AppSetting
	err   error
	calls int
}

func (f *fakeStore) ListAppSettings(context.Context) ([]store.AppSetting, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func row(key, value string) store.AppSetting {
	return store.AppSetting{Key: key, Value: value}
}

func TestAccessorsUseStoredValues(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{
		row(KeyPRDLabel, "Feature"),
		row(KeyAutopilotLabel, "hands-off"),
	}}
	c := New(fs, time.Minute)

	if got, err := c.PRDLabel(context.Background()); err != nil || got != "Feature" {
		t.Fatalf("PRDLabel = %q, %v; want Feature", got, err)
	}
	if got, err := c.AutopilotLabel(context.Background()); err != nil || got != "hands-off" {
		t.Fatalf("AutopilotLabel = %q, %v; want hands-off", got, err)
	}
}

func TestAccessorsFallBackToDefaults(t *testing.T) {
	// Empty table: every accessor yields the compiled-in default, no error.
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.PRDLabel(context.Background()); err != nil || got != DefaultPRDLabel {
		t.Fatalf("PRDLabel = %q, %v; want default %q", got, err, DefaultPRDLabel)
	}
	if got, err := c.AutopilotLabel(context.Background()); err != nil || got != DefaultAutopilotLabel {
		t.Fatalf("AutopilotLabel = %q, %v; want default %q", got, err, DefaultAutopilotLabel)
	}

	// A present-but-empty value is treated as missing and falls back too.
	c = New(&fakeStore{rows: []store.AppSetting{row(KeyPRDLabel, "")}}, time.Minute)
	if got, _ := c.PRDLabel(context.Background()); got != DefaultPRDLabel {
		t.Fatalf("empty stored value: PRDLabel = %q; want default", got)
	}
}

func TestAllReturnsStableShape(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{row(KeyPRDLabel, "Feature")}}
	c := New(fs, time.Minute)
	all, err := c.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != len(Defaults) {
		t.Fatalf("All returned %d keys, want %d", len(all), len(Defaults))
	}
	if all[KeyPRDLabel] != "Feature" {
		t.Errorf("prd_label = %q, want Feature", all[KeyPRDLabel])
	}
	if all[KeyAutopilotLabel] != DefaultAutopilotLabel {
		t.Errorf("autopilot_label = %q, want default", all[KeyAutopilotLabel])
	}
}

func TestCacheServesWithinTTLAndRefetchesAfter(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{row(KeyPRDLabel, "v1")}}
	now := time.Unix(0, 0)
	c := New(fs, time.Minute)
	c.now = func() time.Time { return now }

	if got, _ := c.PRDLabel(context.Background()); got != "v1" {
		t.Fatalf("first read = %q", got)
	}
	// A second read inside the TTL is served from cache: no extra store call,
	// and it does not observe the store's changed rows.
	fs.rows = []store.AppSetting{row(KeyPRDLabel, "v2")}
	if got, _ := c.PRDLabel(context.Background()); got != "v1" {
		t.Fatalf("cached read = %q, want v1 (stale within TTL)", got)
	}
	if fs.calls != 1 {
		t.Fatalf("store called %d times within TTL, want 1", fs.calls)
	}

	// Past the TTL the cache refetches and sees the new value.
	now = now.Add(2 * time.Minute)
	if got, _ := c.PRDLabel(context.Background()); got != "v2" {
		t.Fatalf("post-TTL read = %q, want v2", got)
	}
	if fs.calls != 2 {
		t.Fatalf("store called %d times, want 2 after TTL expiry", fs.calls)
	}
}

func TestInvalidateForcesRefetch(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{row(KeyPRDLabel, "v1")}}
	c := New(fs, time.Hour) // long TTL: only Invalidate should trigger a refetch

	c.PRDLabel(context.Background())
	fs.rows = []store.AppSetting{row(KeyPRDLabel, "v2")}

	// Still cached before invalidation.
	if got, _ := c.PRDLabel(context.Background()); got != "v1" || fs.calls != 1 {
		t.Fatalf("before invalidate: got %q calls %d; want v1/1", got, fs.calls)
	}

	c.Invalidate()
	if got, _ := c.PRDLabel(context.Background()); got != "v2" || fs.calls != 2 {
		t.Fatalf("after invalidate: got %q calls %d; want v2/2", got, fs.calls)
	}
}

func TestStaleOnRefreshError(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{row(KeyPRDLabel, "v1")}}
	now := time.Unix(0, 0)
	c := New(fs, time.Minute)
	c.now = func() time.Time { return now }

	c.PRDLabel(context.Background()) // prime the cache

	// TTL expires and the next refresh errors: the last known-good value is
	// served and no error surfaces.
	now = now.Add(2 * time.Minute)
	fs.err = errors.New("db down")
	if got, err := c.PRDLabel(context.Background()); got != "v1" || err != nil {
		t.Fatalf("stale-on-error: got %q, %v; want v1, nil", got, err)
	}
}

func TestColdErrorReturnsDefaultAndError(t *testing.T) {
	fs := &fakeStore{err: errors.New("db down")}
	c := New(fs, time.Minute)
	got, err := c.PRDLabel(context.Background())
	if err == nil {
		t.Fatal("cold cache with a store error should propagate the error")
	}
	if got != DefaultPRDLabel {
		t.Fatalf("cold error value = %q, want default %q", got, DefaultPRDLabel)
	}
}

func TestValidateLabel(t *testing.T) {
	if err := ValidateLabel("PRD"); err != nil {
		t.Errorf("plain label rejected: %v", err)
	}
	if err := ValidateLabel(strings.Repeat("x", maxLabelLen)); err != nil {
		t.Errorf("exactly %d chars rejected: %v", maxLabelLen, err)
	}
	rejects := map[string]string{
		"empty":           "",
		"whitespace only": "   ",
		"too long":        strings.Repeat("x", maxLabelLen+1),
		"comma":           "a,b",
	}
	for name, v := range rejects {
		if err := ValidateLabel(v); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestValidateMergedRejectsEqualLabels(t *testing.T) {
	if err := ValidateMerged(map[string]string{
		KeyPRDLabel:       "same",
		KeyAutopilotLabel: "same",
	}); err == nil {
		t.Error("equal prd_label and autopilot_label should be rejected")
	}
	if err := ValidateMerged(map[string]string{
		KeyPRDLabel:       "PRD",
		KeyAutopilotLabel: "autopilot",
	}); err != nil {
		t.Errorf("distinct labels rejected: %v", err)
	}
}
