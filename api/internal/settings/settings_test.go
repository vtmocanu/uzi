package settings

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
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
		row(KeyUziLabel, "Feature"),
		row(KeyAutopilotLabel, "hands-off"),
	}}
	c := New(fs, time.Minute)

	if got, err := c.UziLabel(context.Background()); err != nil || got != "Feature" {
		t.Fatalf("UziLabel = %q, %v; want Feature", got, err)
	}
	if got, err := c.AutopilotLabel(context.Background()); err != nil || got != "hands-off" {
		t.Fatalf("AutopilotLabel = %q, %v; want hands-off", got, err)
	}
}

func TestAccessorsFallBackToDefaults(t *testing.T) {
	// Empty table: every accessor yields the compiled-in default, no error.
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.UziLabel(context.Background()); err != nil || got != DefaultUziLabel {
		t.Fatalf("UziLabel = %q, %v; want default %q", got, err, DefaultUziLabel)
	}
	if got, err := c.AutopilotLabel(context.Background()); err != nil || got != DefaultAutopilotLabel {
		t.Fatalf("AutopilotLabel = %q, %v; want default %q", got, err, DefaultAutopilotLabel)
	}

	// A present-but-empty value is treated as missing and falls back too.
	c = New(&fakeStore{rows: []store.AppSetting{row(KeyUziLabel, "")}}, time.Minute)
	if got, _ := c.UziLabel(context.Background()); got != DefaultUziLabel {
		t.Fatalf("empty stored value: UziLabel = %q; want default", got)
	}
}

// TestFindingLabelAccessor pins the PRD #333 D5 marker accessor: a stored value wins,
// an empty table falls back to DefaultFindingLabel, and a present-but-empty row is
// treated as missing — the same read semantics as UziLabel.
func TestFindingLabelAccessor(t *testing.T) {
	// Stored value wins.
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyFindingLabel, "bug-radar")}}, time.Minute)
	if got, err := c.FindingLabel(context.Background()); err != nil || got != "bug-radar" {
		t.Fatalf("FindingLabel = %q, %v; want bug-radar", got, err)
	}
	// Empty table → compiled-in default.
	c = New(&fakeStore{}, time.Minute)
	if got, err := c.FindingLabel(context.Background()); err != nil || got != DefaultFindingLabel {
		t.Fatalf("FindingLabel = %q, %v; want default %q", got, err, DefaultFindingLabel)
	}
	// A present-but-empty value falls back too.
	c = New(&fakeStore{rows: []store.AppSetting{row(KeyFindingLabel, "")}}, time.Minute)
	if got, _ := c.FindingLabel(context.Background()); got != DefaultFindingLabel {
		t.Fatalf("empty stored value: FindingLabel = %q; want default", got)
	}
}

// TestFindingLabelValidationAndShape pins that finding_label is a KNOWN key that goes
// through All/defaults and that its write validation is the single-label rule the other
// label keys use (Validate's default branch → ValidateLabel): non-empty, ≤64 bytes, no comma.
func TestFindingLabelValidationAndShape(t *testing.T) {
	if !Known(KeyFindingLabel) {
		t.Fatal("finding_label is not Known — a settings PUT would reject it")
	}
	all, err := New(&fakeStore{}, time.Minute).All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if all[KeyFindingLabel] != DefaultFindingLabel {
		t.Errorf("All[finding_label] = %q, want default %q", all[KeyFindingLabel], DefaultFindingLabel)
	}
	// A valid label saves.
	if err := Validate(KeyFindingLabel, "agent-found"); err != nil {
		t.Errorf("Validate(finding_label, valid) = %v, want nil", err)
	}
	// The single-label rules apply: empty/whitespace, a comma, and an over-64-char value
	// are each rejected (mirroring uzi_label).
	for _, bad := range []string{"", "   ", "a,b", strings.Repeat("x", maxLabelLen+1)} {
		if err := Validate(KeyFindingLabel, bad); err == nil {
			t.Errorf("Validate(finding_label, %q) = nil, want a label rejection", bad)
		}
	}
}

func TestJudgeAccessors(t *testing.T) {
	// Empty table → compiled-in defaults: OFF, strong opus model (PRD #69 Decision 1).
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.JudgeEnabled(context.Background()); err != nil || got != false {
		t.Fatalf("JudgeEnabled default = %v, %v; want false", got, err)
	}
	if got, err := c.JudgeModel(context.Background()); err != nil || got != DefaultJudgeModel {
		t.Fatalf("JudgeModel default = %q, %v; want %q", got, err, DefaultJudgeModel)
	}
	// Pin the literal default so an accidental flip of DefaultJudgeModel is caught
	// (PRD #69 Decision 1: opus, not the superseded haiku/sonnet).
	if DefaultJudgeModel != "opus" {
		t.Fatalf("DefaultJudgeModel = %q, want \"opus\"", DefaultJudgeModel)
	}

	// "true"/"false" honored; any other value falls back to the default (false) —
	// a malformed row never silently turns token spend ON.
	for _, tc := range []struct {
		stored string
		want   bool
	}{
		{"true", true},
		{"false", false},
		{"", false},       // empty → default false
		{"banana", false}, // junk → default false
		{"TRUE", false},   // non-canonical → default, not a lenient parse
		{"1", false},
	} {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeyJudgeEnabled, tc.stored)}}, time.Minute)
		if got, _ := c.JudgeEnabled(context.Background()); got != tc.want {
			t.Errorf("JudgeEnabled(stored=%q) = %v, want %v", tc.stored, got, tc.want)
		}
	}

	// A configured model is returned verbatim.
	c = New(&fakeStore{rows: []store.AppSetting{row(KeyJudgeModel, "sonnet")}}, time.Minute)
	if got, _ := c.JudgeModel(context.Background()); got != "sonnet" {
		t.Fatalf("JudgeModel = %q, want sonnet", got)
	}

	// JudgeEnforceAll (PRD #69): default OFF, "true"/"false" honored, junk → false —
	// a malformed row never silently turns forced token spend ON.
	if got, err := c.JudgeEnforceAll(context.Background()); err != nil || got != false {
		t.Fatalf("JudgeEnforceAll default = %v, %v; want false", got, err)
	}
	for _, tc := range []struct {
		stored string
		want   bool
	}{
		{"true", true},
		{"false", false},
		{"", false},     // empty → default false
		{"yes", false},  // junk → default false
		{"TRUE", false}, // non-canonical → default, not a lenient parse
		{"1", false},
	} {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeyJudgeEnforceAll, tc.stored)}}, time.Minute)
		if got, _ := c.JudgeEnforceAll(context.Background()); got != tc.want {
			t.Errorf("JudgeEnforceAll(stored=%q) = %v, want %v", tc.stored, got, tc.want)
		}
	}
}

// TestMrReworkAccessors pins the PRD #700 M5 admin gates (Decision 5): the
// kill-switch defaults ON (the OPPOSITE of the judge's default-off), the cap defaults
// to 5, a stored value is honored, and a junk bool row falls back to the default ON.
// The cap returns a PARSE ERROR on a junk row rather than silently reading 5.
func TestMrReworkAccessors(t *testing.T) {
	ctx := context.Background()

	// Empty table → compiled-in defaults: ON and cap 5.
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.MrReworkEnabled(ctx); err != nil || got != true {
		t.Fatalf("MrReworkEnabled default = %v, %v; want true (absent → ON)", got, err)
	}
	if got, err := c.MrReworkCap(ctx); err != nil || got != 5 {
		t.Fatalf("MrReworkCap default = %d, %v; want 5", got, err)
	}
	// Pin the literal defaults so an accidental flip is caught: default-ON is an
	// announced behavior change, and a silent flip to off would be a regression.
	if DefaultMrReworkEnabled != "true" || DefaultMrReworkCap != "5" {
		t.Fatalf("defaults = (%q, %q), want (\"true\", \"5\")", DefaultMrReworkEnabled, DefaultMrReworkCap)
	}

	// present-true / present-false honored; any OTHER value → default ON. A malformed
	// row never silently turns a default-on feature off.
	for _, tc := range []struct {
		stored string
		want   bool
	}{
		{"true", true},
		{"false", false},
		{"", true},       // empty → default ON
		{"banana", true}, // junk → default ON
		{"TRUE", true},   // non-canonical → default, not a lenient parse
		{"0", true},
	} {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeyMrReworkEnabled, tc.stored)}}, time.Minute)
		if got, _ := c.MrReworkEnabled(ctx); got != tc.want {
			t.Errorf("MrReworkEnabled(stored=%q) = %v, want %v", tc.stored, got, tc.want)
		}
	}

	// A set cap is honored; a blank/whitespace row falls to the default; a junk row
	// (hand-edited, bypassing write validation) returns a PARSE ERROR the caller
	// decides on — NOT a silent fallback to 5.
	for _, tc := range []struct {
		stored  string
		want    int
		wantErr bool
	}{
		{"3", 3, false},
		{"10", 10, false},
		{"", 5, false},    // empty → default 5
		{"   ", 5, false}, // whitespace → default 5
		{"abc", 0, true},  // junk → parse error returned, not silently 5
	} {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeyMrReworkCap, tc.stored)}}, time.Minute)
		got, err := c.MrReworkCap(ctx)
		if tc.wantErr && err == nil {
			t.Errorf("MrReworkCap(stored=%q) err = nil, want a parse error", tc.stored)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("MrReworkCap(stored=%q) err = %v, want nil", tc.stored, err)
		}
		if got != tc.want {
			t.Errorf("MrReworkCap(stored=%q) = %d, want %d", tc.stored, got, tc.want)
		}
	}
}

// TestMrReworkEnabledFailClosed pins Decision 5's fail-closed reconciliation: a
// genuine store READ ERROR must be PROPAGATED, not collapsed into the value. An
// absent row reads ON (default), but the caller (the M3 detector) must be able to
// tell an error apart from absent so it maps error → OFF. This exercises a REAL
// cold-cache read error, not merely an absent row (the fail-open trap R3 named).
func TestMrReworkEnabledFailClosed(t *testing.T) {
	ctx := context.Background()

	// Cold cache + store error: the reader RETURNS the error so the caller fails
	// closed. Contrast with the absent-row case above, which returns (true, nil).
	c := New(&fakeStore{err: errors.New("db down")}, time.Minute)
	if _, err := c.MrReworkEnabled(ctx); err == nil {
		t.Fatal("MrReworkEnabled on a cold store error must propagate the error (caller maps it to OFF)")
	}
	if _, err := c.MrReworkCap(ctx); err == nil {
		t.Fatal("MrReworkCap on a cold store error must propagate the error")
	}
}

// TestMrReworkValidation pins the PRD #700 M5 write-time gates: the enabled key
// routes to the strict bool parse; the cap routes to the [1, maxMrReworkCap] integer
// gate. Both MUST have explicit Validate cases — the default branch (ValidateLabel)
// would accept junk that then reads as the default (enabled) or a per-read parse
// error (cap).
func TestMrReworkValidation(t *testing.T) {
	for _, ok := range []string{"true", "false"} {
		if err := Validate(KeyMrReworkEnabled, ok); err != nil {
			t.Errorf("Validate(mr_rework_enabled, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "yes", "1", "TRUE", "on"} {
		if err := Validate(KeyMrReworkEnabled, bad); err == nil {
			t.Errorf("Validate(mr_rework_enabled, %q) = nil, want a non-bool rejection", bad)
		}
	}
	// cap: [1, 100] accepted at the edges; 0 (use the kill-switch instead),
	// negatives, non-ints, over-cap, and empty are rejected.
	for _, ok := range []string{"1", "5", "100"} {
		if err := Validate(KeyMrReworkCap, ok); err != nil {
			t.Errorf("Validate(mr_rework_cap, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"0", "-1", "101", "abc", ""} {
		if err := Validate(KeyMrReworkCap, bad); err == nil {
			t.Errorf("Validate(mr_rework_cap, %q) = nil, want a rejection", bad)
		}
	}
	for _, k := range []string{KeyMrReworkEnabled, KeyMrReworkCap} {
		if !Known(k) {
			t.Errorf("%s should be Known (admin-writable)", k)
		}
	}
}

// TestJudgeSpendGuardAccessors pins the PRD #69 M5 Decision 9 per-user spend-guard
// accessors: the cooldown defaults ON (60s), the daily budget OFF (0), and a stored
// value is returned verbatim while an unparseable row falls back to the default
// (the same junk-tolerance the other int accessors carry).
func TestJudgeSpendGuardAccessors(t *testing.T) {
	// Empty table → compiled-in defaults.
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.JudgeCooldownSeconds(context.Background()); err != nil || got != 60 {
		t.Fatalf("JudgeCooldownSeconds default = %d, %v; want 60", got, err)
	}
	if got, err := c.JudgeDailyBudget(context.Background()); err != nil || got != 0 {
		t.Fatalf("JudgeDailyBudget default = %d, %v; want 0", got, err)
	}
	// Pin the literal defaults so an accidental flip is caught.
	if DefaultJudgeCooldownSeconds != "60" || DefaultJudgeDailyBudget != "0" {
		t.Fatalf("defaults = (%q, %q), want (\"60\", \"0\")", DefaultJudgeCooldownSeconds, DefaultJudgeDailyBudget)
	}

	// Stored values returned verbatim; a junk row falls back to the default.
	for _, tc := range []struct {
		stored string
		want   int
	}{
		{"0", 0}, {"120", 120}, {"86400", 86400}, {"", 60}, {"banana", 60},
	} {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeyJudgeCooldownSeconds, tc.stored)}}, time.Minute)
		if got, _ := c.JudgeCooldownSeconds(context.Background()); got != tc.want {
			t.Errorf("JudgeCooldownSeconds(stored=%q) = %d, want %d", tc.stored, got, tc.want)
		}
	}
	for _, tc := range []struct {
		stored string
		want   int
	}{
		{"0", 0}, {"25", 25}, {"", 0}, {"junk", 0},
	} {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeyJudgeDailyBudget, tc.stored)}}, time.Minute)
		if got, _ := c.JudgeDailyBudget(context.Background()); got != tc.want {
			t.Errorf("JudgeDailyBudget(stored=%q) = %d, want %d", tc.stored, got, tc.want)
		}
	}
}

// TestJudgeSpendGuardValidation pins the PRD #69 M5 Decision 9 write-time bounds: the
// cooldown reuses the run-health {0} ∪ [60, 86400] gate; the budget is 0 (unlimited)
// or a positive count under the cap. Both MUST have explicit int cases — the default
// branch (ValidateLabel) would accept junk that then reads as the default.
func TestJudgeSpendGuardValidation(t *testing.T) {
	// Cooldown: 0 disables; the [60, 86400] band is accepted at both edges; a sub-60
	// value, a non-int, and an over-cap value are rejected.
	for _, ok := range []string{"0", "60", "86400", "3600"} {
		if err := Validate(KeyJudgeCooldownSeconds, ok); err != nil {
			t.Errorf("Validate(judge_cooldown_seconds, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"59", "30", "1", "86401", "-1", "abc", ""} {
		if err := Validate(KeyJudgeCooldownSeconds, bad); err == nil {
			t.Errorf("Validate(judge_cooldown_seconds, %q) = nil, want a rejection", bad)
		}
	}

	// Budget: 0 (unlimited) and a positive count accepted; negatives, non-ints, and
	// an absurd over-cap value rejected.
	for _, ok := range []string{"0", "1", "50", "10000"} {
		if err := Validate(KeyJudgeDailyBudget, ok); err != nil {
			t.Errorf("Validate(judge_daily_budget, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"-1", "-5", "abc", "", "10001"} {
		if err := Validate(KeyJudgeDailyBudget, bad); err == nil {
			t.Errorf("Validate(judge_daily_budget, %q) = nil, want a rejection", bad)
		}
	}
}

// TestSummaryModelAccessor pins PRD #362 Decision 8 at the settings layer: the
// run-summary model defaults to "haiku" (unlike the judge's "opus"), a stored value
// is returned verbatim, the key is Known/writable, and validation accepts an alias
// and a full id but rejects blank/over-long/garbage — reusing validateModelAlias,
// exactly the judge_model validator.
func TestSummaryModelAccessor(t *testing.T) {
	// Empty table → the compiled-in cheap default.
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.SummaryModel(context.Background()); err != nil || got != DefaultSummaryModel {
		t.Fatalf("SummaryModel default = %q, %v; want %q", got, err, DefaultSummaryModel)
	}
	// Pin the literal default so an accidental flip is caught (Decision 8: haiku, the
	// cheap per-run default, NOT the judge's opus).
	if DefaultSummaryModel != "haiku" {
		t.Fatalf("DefaultSummaryModel = %q, want \"haiku\"", DefaultSummaryModel)
	}

	// A configured value is returned verbatim.
	c = New(&fakeStore{rows: []store.AppSetting{row(KeySummaryModel, "sonnet")}}, time.Minute)
	if got, _ := c.SummaryModel(context.Background()); got != "sonnet" {
		t.Fatalf("SummaryModel = %q, want sonnet", got)
	}

	// Writable through the settings PUT.
	if !Known(KeySummaryModel) {
		t.Errorf("%s should be Known (admin-writable)", KeySummaryModel)
	}

	// Validation accepts an alias and a full id; rejects blank, whitespace, and over-long.
	for _, ok := range []string{"haiku", "sonnet", "opus", "fable", "claude-3-5-haiku-20241022"} {
		if err := Validate(KeySummaryModel, ok); err != nil {
			t.Errorf("Validate(%s, %q) = %v, want nil", KeySummaryModel, ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "two words", strings.Repeat("x", 101)} {
		if err := Validate(KeySummaryModel, bad); err == nil {
			t.Errorf("Validate(%s, %q) = nil, want an error", KeySummaryModel, bad)
		}
	}
}

// TestJudgeWritability pins Decision 7 at the settings layer: the judge keys are
// Known (writable through the settings PUT) with per-value validation. (The
// selfimprove keys and their validation were retired with the engine in PRD #590 M2.)
func TestJudgeWritability(t *testing.T) {
	writable := map[string]string{
		KeyJudgeEnabled:    "true",
		KeyJudgeEnforceAll: "false",
		KeyJudgeModel:      "haiku",
	}
	for k, v := range writable {
		if !Known(k) {
			t.Errorf("%s should be Known (admin-writable)", k)
		}
		if err := Validate(k, v); err != nil {
			t.Errorf("Validate(%s, %q) = %v, want nil", k, v, err)
		}
	}

	// Per-value validation rejects the obvious bad inputs.
	for _, tc := range []struct{ key, value string }{
		{KeyJudgeEnabled, "yes"},
		{KeyJudgeModel, ""},
		{KeyJudgeModel, "two words"},
	} {
		if err := Validate(tc.key, tc.value); err == nil {
			t.Errorf("Validate(%s, %q) = nil, want an error", tc.key, tc.value)
		}
	}
}

func TestAllReturnsStableShape(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{row(KeyUziLabel, "Feature")}}
	c := New(fs, time.Minute)
	all, err := c.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != len(Defaults) {
		t.Fatalf("All returned %d keys, want %d", len(all), len(Defaults))
	}
	if all[KeyUziLabel] != "Feature" {
		t.Errorf("uzi_label = %q, want Feature", all[KeyUziLabel])
	}
	if all[KeyAutopilotLabel] != DefaultAutopilotLabel {
		t.Errorf("autopilot_label = %q, want default", all[KeyAutopilotLabel])
	}
}

func TestCacheServesWithinTTLAndRefetchesAfter(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{row(KeyUziLabel, "v1")}}
	now := time.Unix(0, 0)
	c := New(fs, time.Minute)
	c.now = func() time.Time { return now }

	if got, _ := c.UziLabel(context.Background()); got != "v1" {
		t.Fatalf("first read = %q", got)
	}
	// A second read inside the TTL is served from cache: no extra store call,
	// and it does not observe the store's changed rows.
	fs.rows = []store.AppSetting{row(KeyUziLabel, "v2")}
	if got, _ := c.UziLabel(context.Background()); got != "v1" {
		t.Fatalf("cached read = %q, want v1 (stale within TTL)", got)
	}
	if fs.calls.Load() != 1 {
		t.Fatalf("store called %d times within TTL, want 1", fs.calls.Load())
	}

	// Past the TTL the cache refetches and sees the new value.
	now = now.Add(2 * time.Minute)
	if got, _ := c.UziLabel(context.Background()); got != "v2" {
		t.Fatalf("post-TTL read = %q, want v2", got)
	}
	if fs.calls.Load() != 2 {
		t.Fatalf("store called %d times, want 2 after TTL expiry", fs.calls.Load())
	}
}

func TestInvalidateForcesRefetch(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{row(KeyUziLabel, "v1")}}
	c := New(fs, time.Hour) // long TTL: only Invalidate should trigger a refetch

	_, _ = c.UziLabel(context.Background())
	fs.rows = []store.AppSetting{row(KeyUziLabel, "v2")}

	// Still cached before invalidation.
	if got, _ := c.UziLabel(context.Background()); got != "v1" || fs.calls.Load() != 1 {
		t.Fatalf("before invalidate: got %q calls %d; want v1/1", got, fs.calls.Load())
	}

	c.Invalidate()
	if got, _ := c.UziLabel(context.Background()); got != "v2" || fs.calls.Load() != 2 {
		t.Fatalf("after invalidate: got %q calls %d; want v2/2", got, fs.calls.Load())
	}
}

func TestStaleOnRefreshError(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{row(KeyUziLabel, "v1")}}
	now := time.Unix(0, 0)
	c := New(fs, time.Minute)
	c.now = func() time.Time { return now }

	_, _ = c.UziLabel(context.Background()) // prime the cache

	// TTL expires and the next refresh errors: the last known-good value is
	// served and no error surfaces.
	now = now.Add(2 * time.Minute)
	fs.err = errors.New("db down")
	if got, err := c.UziLabel(context.Background()); got != "v1" || err != nil {
		t.Fatalf("stale-on-error: got %q, %v; want v1, nil", got, err)
	}
}

func TestColdErrorReturnsDefaultAndError(t *testing.T) {
	fs := &fakeStore{err: errors.New("db down")}
	c := New(fs, time.Minute)
	got, err := c.UziLabel(context.Background())
	if err == nil {
		t.Fatal("cold cache with a store error should propagate the error")
	}
	if got != DefaultUziLabel {
		t.Fatalf("cold error value = %q, want default %q", got, DefaultUziLabel)
	}
}

// TestConcurrentAccess exercises the RWMutex guard: many readers (the poller +
// handlers pattern) racing writes/invalidations. Run under -race to catch an
// unguarded field. A short TTL forces frequent refreshes so the fast and slow
// paths both run concurrently.
func TestConcurrentAccess(t *testing.T) {
	fs := &fakeStore{rows: []store.AppSetting{
		row(KeyUziLabel, "PRD"),
		row(KeyAutopilotLabel, "autopilot"),
	}}
	c := New(fs, time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = c.UziLabel(context.Background())
				_, _ = c.All(context.Background())
				if j%20 == 0 {
					c.Invalidate()
				}
			}
		}()
	}
	wg.Wait()
}

func TestDefaultThemeAccessor(t *testing.T) {
	// A stored value wins.
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyDefaultTheme, "mission")}}, time.Minute)
	if got, err := c.DefaultTheme(context.Background()); err != nil || got != "mission" {
		t.Fatalf("DefaultTheme = %q, %v; want mission", got, err)
	}
	// An empty table falls back to the registry default ("ember"), so an instance
	// that never set a theme renders the original look.
	c = New(&fakeStore{}, time.Minute)
	if got, err := c.DefaultTheme(context.Background()); err != nil || got != "ember" {
		t.Fatalf("DefaultTheme = %q, %v; want default ember", got, err)
	}
}

func TestValidateDispatch(t *testing.T) {
	// Label keys route to the label rules: a comma is rejected.
	if err := Validate(KeyUziLabel, "a,b"); err == nil {
		t.Error("Validate(uzi_label, \"a,b\") = nil, want a label rejection")
	}
	if err := Validate(KeyAutopilotLabel, "autopilot"); err != nil {
		t.Errorf("Validate(autopilot_label, valid) = %v, want nil", err)
	}
	// default_theme routes to the theme registry, not the label rules.
	if err := Validate(KeyDefaultTheme, "mission"); err != nil {
		t.Errorf("Validate(default_theme, mission) = %v, want nil", err)
	}
	if err := Validate(KeyDefaultTheme, "neon"); err == nil {
		t.Error("Validate(default_theme, neon) = nil, want an unknown-theme rejection")
	}
	// A theme value that a label rule would ACCEPT (no comma, short) must still be
	// rejected by the theme registry — proving the dispatch, not just length.
	if err := Validate(KeyDefaultTheme, "PRD"); err == nil {
		t.Error("Validate(default_theme, \"PRD\") = nil; a valid label is not a valid theme")
	}
}

func TestLabelChanged(t *testing.T) {
	committed := map[string]string{
		KeyUziLabel:       "uzi",
		KeyAutopilotLabel: "autopilot",
		KeyDefaultTheme:   "ember",
	}
	cases := []struct {
		name    string
		updates map[string]string
		want    bool
	}{
		// PRD #764: the board now filters on the uzi label, so a uzi_label change forces a resync.
		{"a uzi_label change forces a resync", map[string]string{KeyUziLabel: "runnable"}, true},
		{"an autopilot_label change forces a resync", map[string]string{KeyAutopilotLabel: "hands-off"}, true},
		{"a theme-only change does NOT force a resync", map[string]string{KeyDefaultTheme: "mission"}, false},
		{"an idempotent label write does not", map[string]string{KeyUziLabel: "uzi"}, false},
		{"label + theme together still resyncs on the label", map[string]string{KeyUziLabel: "runnable", KeyDefaultTheme: "mission"}, true},
		{"an empty update does not", map[string]string{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LabelChanged(committed, c.updates); got != c.want {
				t.Fatalf("LabelChanged(%v) = %v, want %v", c.updates, got, c.want)
			}
		})
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

func TestEffective(t *testing.T) {
	// A stored non-empty value overrides the default; an absent key and a
	// present-but-empty value both fall back; an unknown-key row is ignored.
	got := Effective([]store.AppSetting{
		row(KeyUziLabel, "Feature"),
		row(KeyAutopilotLabel, ""), // empty → default
		row("bogus", "x"),          // unknown → ignored
	})
	if len(got) != len(Defaults) {
		t.Fatalf("Effective returned %d keys, want %d (only known keys)", len(got), len(Defaults))
	}
	if got[KeyUziLabel] != "Feature" {
		t.Errorf("uzi_label = %q, want Feature", got[KeyUziLabel])
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
// writer already committed uzi_label="autopilot"; a second PUT setting
// autopilot_label="autopilot" must be rejected — even though a cache still showing
// the seeded default uzi_label="uzi" would have wrongly accepted it.
func TestEffectiveDrivesMergedValidationFromCommittedRows(t *testing.T) {
	pending := map[string]string{KeyAutopilotLabel: "hands-off"}

	// Against the committed rows (uzi_label already "hands-off"), the merge collides.
	committed := Effective([]store.AppSetting{
		row(KeyUziLabel, "hands-off"),
		row(KeyAutopilotLabel, DefaultAutopilotLabel),
	})
	for k, v := range pending {
		committed[k] = v
	}
	if err := ValidateMerged(committed); err == nil {
		t.Fatal("expected rejection: committed uzi_label==autopilot_label after merge")
	}

	// Against a stale cache view (uzi_label still the default "uzi"), the same merge
	// would have passed — proving the committed rows, not the cache, decide it.
	stale := Effective([]store.AppSetting{
		row(KeyUziLabel, DefaultUziLabel),
		row(KeyAutopilotLabel, DefaultAutopilotLabel),
	})
	for k, v := range pending {
		stale[k] = v
	}
	if err := ValidateMerged(stale); err != nil {
		t.Fatalf("stale view unexpectedly rejected (%v); the accept/reject contrast is the proof", err)
	}
}

// TestValidateMergedUziLabelDistinctFromAutopilot covers PRD #764: the
// run-eligibility label must differ from the autopilot label (an equal pair would
// autopilot every runnable issue). The rejection names uzi_label. Sharing an
// arbitrary organizational label (e.g. a plain "PRD") is allowed.
func TestValidateMergedUziLabelDistinctFromAutopilot(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{
			KeyAutopilotLabel: "autopilot",
			KeyUziLabel:       "uzi",
		}
	}

	m := base()
	m[KeyUziLabel] = "autopilot"
	if err := ValidateMerged(m); err == nil || !strings.Contains(err.Error(), "uzi_label") {
		t.Errorf("uzi_label==autopilot_label: err = %v, want a rejection naming uzi_label", err)
	}

	// uzi_label sharing a plain organizational label is allowed — no distinctness rule
	// binds it to anything but the autopilot label.
	m = base()
	m[KeyUziLabel] = "PRD"
	if err := ValidateMerged(m); err != nil {
		t.Errorf("uzi_label sharing a plain label must be allowed: %v", err)
	}

	// The shipped defaults are mutually distinct and pass.
	if err := ValidateMerged(base()); err != nil {
		t.Errorf("distinct default labels rejected: %v", err)
	}
}

// TestUziLabelAccessorAndDefault pins PRD #764's no-seeded-row synthesis: with no
// row present the accessor returns the compiled-in default, and a stored row overrides
// it.
func TestUziLabelAccessorAndDefault(t *testing.T) {
	if DefaultUziLabel != "uzi" {
		t.Fatalf("DefaultUziLabel = %q, want \"uzi\"", DefaultUziLabel)
	}
	if Defaults[KeyUziLabel] != DefaultUziLabel {
		t.Fatalf("Defaults[uzi_label] = %q, want %q (no-seeded-row synthesis)", Defaults[KeyUziLabel], DefaultUziLabel)
	}

	// Empty store → default.
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.UziLabel(context.Background()); err != nil || got != DefaultUziLabel {
		t.Fatalf("UziLabel() empty = (%q, %v), want (%q, nil)", got, err, DefaultUziLabel)
	}

	// Stored row wins.
	c2 := New(&fakeStore{rows: []store.AppSetting{{Key: KeyUziLabel, Value: "runnable"}}}, time.Minute)
	if got, err := c2.UziLabel(context.Background()); err != nil || got != "runnable" {
		t.Fatalf("UziLabel() stored = (%q, %v), want (\"runnable\", nil)", got, err)
	}
}
