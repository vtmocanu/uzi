package settings

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeStore is an in-memory Store: it returns a fixed row set and counts calls
// so the cache's refresh behavior is observable, and can be flipped to error.
// calls is atomic so the concurrent test can race readers through it without the
// fake itself tripping -race.
type fakeStore struct {
	rows  []store.AppSetting
	err   error
	calls atomic.Int64
}

func (f *fakeStore) ListAppSettings(context.Context) ([]store.AppSetting, error) {
	f.calls.Add(1)
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

func TestPrdlessAccessors(t *testing.T) {
	// Empty table → compiled-in defaults: enabled (true) + "PRDLESS".
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.PrdlessEnabled(context.Background()); err != nil || got != true {
		t.Fatalf("PrdlessEnabled default = %v, %v; want true", got, err)
	}
	if got, err := c.PrdlessLabel(context.Background()); err != nil || got != DefaultPrdlessLabel {
		t.Fatalf("PrdlessLabel default = %q, %v; want %q", got, err, DefaultPrdlessLabel)
	}

	// "true"/"false" are honored verbatim; every other value (empty, junk, or a
	// non-canonical spelling only reachable through the M1-deferred PUT) falls back
	// to the compiled-in default (true) rather than silently reading as false.
	for _, tc := range []struct {
		stored string
		want   bool
	}{
		{"true", true},
		{"false", false},
		{"", true},       // empty → default "true"
		{"banana", true}, // junk → compiled-in default, NOT false
		{"TRUE", true},   // non-canonical spelling → default, not a lenient parse
		{"0", true},      // ditto
	} {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeyPrdlessEnabled, tc.stored)}}, time.Minute)
		if got, _ := c.PrdlessEnabled(context.Background()); got != tc.want {
			t.Errorf("PrdlessEnabled(stored=%q) = %v, want %v", tc.stored, got, tc.want)
		}
	}

	// A configured custom label is returned verbatim.
	c = New(&fakeStore{rows: []store.AppSetting{row(KeyPrdlessLabel, "NOSPEC")}}, time.Minute)
	if got, _ := c.PrdlessLabel(context.Background()); got != "NOSPEC" {
		t.Fatalf("PrdlessLabel = %q, want NOSPEC", got)
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
	if fs.calls.Load() != 1 {
		t.Fatalf("store called %d times within TTL, want 1", fs.calls.Load())
	}

	// Past the TTL the cache refetches and sees the new value.
	now = now.Add(2 * time.Minute)
	if got, _ := c.PRDLabel(context.Background()); got != "v2" {
		t.Fatalf("post-TTL read = %q, want v2", got)
	}
	if fs.calls.Load() != 2 {
		t.Fatalf("store called %d times, want 2 after TTL expiry", fs.calls.Load())
	}
}

func TestInvalidateForcesRefetch(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{row(KeyPRDLabel, "v1")}}
	c := New(fs, time.Hour) // long TTL: only Invalidate should trigger a refetch

	c.PRDLabel(context.Background())
	fs.rows = []store.AppSetting{row(KeyPRDLabel, "v2")}

	// Still cached before invalidation.
	if got, _ := c.PRDLabel(context.Background()); got != "v1" || fs.calls.Load() != 1 {
		t.Fatalf("before invalidate: got %q calls %d; want v1/1", got, fs.calls.Load())
	}

	c.Invalidate()
	if got, _ := c.PRDLabel(context.Background()); got != "v2" || fs.calls.Load() != 2 {
		t.Fatalf("after invalidate: got %q calls %d; want v2/2", got, fs.calls.Load())
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

// TestConcurrentAccess exercises the RWMutex guard: many readers (the poller +
// handlers pattern) racing writes/invalidations. Run under -race to catch an
// unguarded field. A short TTL forces frequent refreshes so the fast and slow
// paths both run concurrently.
func TestConcurrentAccess(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{
		row(KeyPRDLabel, "PRD"),
		row(KeyAutopilotLabel, "autopilot"),
	}}
	c := New(fs, time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = c.PRDLabel(context.Background())
				_, _ = c.All(context.Background())
				if j%20 == 0 {
					c.Invalidate()
				}
			}
		}()
	}
	wg.Wait()
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

func TestEffective(t *testing.T) {
	// A stored non-empty value overrides the default; an absent key and a
	// present-but-empty value both fall back; an unknown-key row is ignored.
	got := Effective([]store.AppSetting{
		row(KeyPRDLabel, "Feature"),
		row(KeyAutopilotLabel, ""), // empty → default
		row("bogus", "x"),          // unknown → ignored
	})
	if len(got) != len(Defaults) {
		t.Fatalf("Effective returned %d keys, want %d (only known keys)", len(got), len(Defaults))
	}
	if got[KeyPRDLabel] != "Feature" {
		t.Errorf("prd_label = %q, want Feature", got[KeyPRDLabel])
	}
	if got[KeyAutopilotLabel] != DefaultAutopilotLabel {
		t.Errorf("empty stored autopilot_label = %q, want default", got[KeyAutopilotLabel])
	}
	if _, ok := got["bogus"]; ok {
		t.Error("unknown key leaked into Effective output")
	}
}

// TestEffectiveDrivesMergedValidationFromCommittedRows encodes the TOCTOU the M2
// PUT closes: the cross-key check must be a function of the committed rows the
// handler reads under FOR UPDATE, not of a stale cache. Scenario: a concurrent
// writer already committed prd_label="Feature"; a second PUT setting
// autopilot_label="Feature" must be rejected — even though a cache still showing
// the seeded default prd_label="PRD" would have wrongly accepted it.
func TestEffectiveDrivesMergedValidationFromCommittedRows(t *testing.T) {
	pending := map[string]string{KeyAutopilotLabel: "Feature"}

	// Against the committed rows (prd_label already "Feature"), the merge collides.
	committed := Effective([]store.AppSetting{
		row(KeyPRDLabel, "Feature"),
		row(KeyAutopilotLabel, DefaultAutopilotLabel),
	})
	for k, v := range pending {
		committed[k] = v
	}
	if err := ValidateMerged(committed); err == nil {
		t.Fatal("expected rejection: committed prd_label==autopilot_label after merge")
	}

	// Against a stale cache view (prd_label still the seeded "PRD"), the same merge
	// would have passed — proving the committed rows, not the cache, decide it.
	stale := Effective([]store.AppSetting{
		row(KeyPRDLabel, DefaultPRDLabel),
		row(KeyAutopilotLabel, DefaultAutopilotLabel),
	})
	for k, v := range pending {
		stale[k] = v
	}
	if err := ValidateMerged(stale); err != nil {
		t.Fatalf("stale view unexpectedly rejected (%v); the accept/reject contrast is the proof", err)
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
