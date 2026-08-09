package settings

import (
	"context"
	"errors"
	"strconv"
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

func TestJudgeAccessors(t *testing.T) {
	// Empty table → compiled-in defaults: OFF, cheap haiku model.
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.JudgeEnabled(context.Background()); err != nil || got != false {
		t.Fatalf("JudgeEnabled default = %v, %v; want false", got, err)
	}
	if got, err := c.JudgeModel(context.Background()); err != nil || got != DefaultJudgeModel {
		t.Fatalf("JudgeModel default = %q, %v; want %q", got, err, DefaultJudgeModel)
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
}

func TestSelfimproveAccessors(t *testing.T) {
	// Empty table → compiled-in defaults: OFF, 48h interval, empty engine state.
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.SelfimproveEnabled(context.Background()); err != nil || got != false {
		t.Fatalf("SelfimproveEnabled default = %v, %v; want false", got, err)
	}
	if got, err := c.SelfimproveInterval(context.Background()); err != nil || got != 48*time.Hour {
		t.Fatalf("SelfimproveInterval default = %v, %v; want 48h", got, err)
	}
	if got, ok, err := c.SelfimproveLastRunAt(context.Background()); err != nil || ok || !got.IsZero() {
		t.Fatalf("SelfimproveLastRunAt unset = (%v, %v, %v); want (zero, false, nil)", got, ok, err)
	}
	if got, _ := c.SelfimproveRepo(context.Background()); got != "" {
		t.Fatalf("SelfimproveRepo unset = %q, want empty", got)
	}
	if got, _ := c.SelfimproveUserID(context.Background()); got != "" {
		t.Fatalf("SelfimproveUserID unset = %q, want empty", got)
	}

	// A stored interval parses; a junk/zero interval falls back to the default so a
	// bad row never yields a fire-every-cycle scheduler.
	for _, tc := range []struct {
		stored string
		want   time.Duration
	}{
		{"24h", 24 * time.Hour},
		{"30m", 30 * time.Minute},
		{"garbage", 48 * time.Hour},
		{"0s", 48 * time.Hour},
		{"-5h", 48 * time.Hour},
	} {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeySelfimproveInterval, tc.stored)}}, time.Minute)
		if got, _ := c.SelfimproveInterval(context.Background()); got != tc.want {
			t.Errorf("SelfimproveInterval(stored=%q) = %v, want %v", tc.stored, got, tc.want)
		}
	}

	// Engine state reads back verbatim; last_run_at parses RFC3339.
	ts := "2026-07-12T10:30:00Z"
	c = New(&fakeStore{rows: []store.AppSetting{
		row(KeySelfimproveRepo, "repo-uuid-1"),
		row(KeySelfimproveUserID, "user-uuid-1"),
		row(KeySelfimproveLastRunAt, ts),
	}}, time.Minute)
	if got, _ := c.SelfimproveRepo(context.Background()); got != "repo-uuid-1" {
		t.Errorf("SelfimproveRepo = %q, want repo-uuid-1", got)
	}
	if got, _ := c.SelfimproveUserID(context.Background()); got != "user-uuid-1" {
		t.Errorf("SelfimproveUserID = %q, want user-uuid-1", got)
	}
	if got, ok, _ := c.SelfimproveLastRunAt(context.Background()); !ok || got.Format(time.RFC3339) != ts {
		t.Errorf("SelfimproveLastRunAt = (%v, %v), want (%s, true)", got, ok, ts)
	}
}

// TestJudgeSelfimproveWritability pins Decision 7/9 + audit H3 at the settings
// layer: the four admin-configurable keys are Known (writable through the settings
// PUT) with per-value validation, while the three engine-managed selfimprove keys
// are deliberately NOT Known — a body PUT to them is rejected as unknown, so
// selfimprove_user_id can never be set from a request.
func TestJudgeSelfimproveWritability(t *testing.T) {
	writable := map[string]string{
		KeyJudgeEnabled:        "true",
		KeyJudgeModel:          "haiku",
		KeySelfimproveEnabled:  "false",
		KeySelfimproveInterval: "48h",
	}
	for k, v := range writable {
		if !Known(k) {
			t.Errorf("%s should be Known (admin-writable)", k)
		}
		if err := Validate(k, v); err != nil {
			t.Errorf("Validate(%s, %q) = %v, want nil", k, v, err)
		}
	}
	for _, k := range []string{KeySelfimproveRepo, KeySelfimproveUserID, KeySelfimproveLastRunAt} {
		if Known(k) {
			t.Errorf("%s must NOT be Known — it is engine-managed, never body-writable (audit H3)", k)
		}
	}

	// Per-value validation rejects the obvious bad inputs.
	for _, tc := range []struct{ key, value string }{
		{KeyJudgeEnabled, "yes"},
		{KeyJudgeModel, ""},
		{KeyJudgeModel, "two words"},
		{KeySelfimproveEnabled, "1"},
		{KeySelfimproveInterval, "soon"},
		{KeySelfimproveInterval, "-1h"},
	} {
		if err := Validate(tc.key, tc.value); err == nil {
			t.Errorf("Validate(%s, %q) = nil, want an error", tc.key, tc.value)
		}
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
	if err := Validate(KeyPRDLabel, "a,b"); err == nil {
		t.Error("Validate(prd_label, \"a,b\") = nil, want a label rejection")
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
	// prdless_enabled routes to the strict bool parse, NOT the label rules — so a
	// value a label rule would accept (short, no comma) is still rejected unless it
	// is exactly "true"/"false" (PRD #22).
	for _, ok := range []string{"true", "false"} {
		if err := Validate(KeyPrdlessEnabled, ok); err != nil {
			t.Errorf("Validate(prdless_enabled, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "banana", "1", "0", "TRUE", "yes"} {
		if err := Validate(KeyPrdlessEnabled, bad); err == nil {
			t.Errorf("Validate(prdless_enabled, %q) = nil, want a non-bool rejection", bad)
		}
	}
	// prdless_label routes to the label rules like the other labels.
	if err := Validate(KeyPrdlessLabel, "a,b"); err == nil {
		t.Error("Validate(prdless_label, \"a,b\") = nil, want a label rejection")
	}
	if err := Validate(KeyPrdlessLabel, "PRDLESS"); err != nil {
		t.Errorf("Validate(prdless_label, valid) = %v, want nil", err)
	}
	// PRD #196: eligible_label_waives_prd_link routes to the strict bool parse —
	// without its arm the default branch (ValidateLabel) would accept "yes"/"maybe"
	// and the gate would fail OPEN.
	for _, ok := range []string{"true", "false"} {
		if err := Validate(KeyEligibleLabelWaivesPRDLink, ok); err != nil {
			t.Errorf("Validate(eligible_label_waives_prd_link, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "yes", "maybe", "PRD", "1", "TRUE"} {
		if err := Validate(KeyEligibleLabelWaivesPRDLink, bad); err == nil {
			t.Errorf("Validate(eligible_label_waives_prd_link, %q) = nil, want a non-bool rejection (fails open otherwise)", bad)
		}
	}
	// PRD #196: the two list keys route to validateLabelList, which ACCEPTS the comma
	// a two-label list needs — the default branch (ValidateLabel) would reject it.
	for _, k := range []string{KeyRunEligibleLabels, KeyBoardExtraLabels} {
		if err := Validate(k, "PRD,bug"); err != nil {
			t.Errorf("Validate(%s, \"PRD,bug\") = %v, want nil (comma list must be accepted)", k, err)
		}
		// A bad token in the list is still rejected (a too-long token; a token can
		// never carry a comma post-split, and a whitespace-only token is dropped).
		if err := Validate(k, "PRD,"+strings.Repeat("x", maxLabelLen+1)); err == nil {
			t.Errorf("Validate(%s, too-long token) = nil, want a bad-token rejection", k)
		}
		// Empty is allowed (no extras / handled by ValidateMerged for eligible).
		if err := Validate(k, ""); err != nil {
			t.Errorf("Validate(%s, \"\") = %v, want nil (empty allowed)", k, err)
		}
	}
}

func TestLabelChanged(t *testing.T) {
	committed := map[string]string{
		KeyPRDLabel:       "PRD",
		KeyAutopilotLabel: "autopilot",
		KeyDefaultTheme:   "ember",
		KeyPrdlessEnabled: "true",
		KeyPrdlessLabel:   "PRDLESS",
	}
	cases := []struct {
		name    string
		updates map[string]string
		want    bool
	}{
		{"a label change forces a resync", map[string]string{KeyPRDLabel: "Feature"}, true},
		{"a theme-only change does NOT force a resync", map[string]string{KeyDefaultTheme: "mission"}, false},
		{"an idempotent label write does not", map[string]string{KeyPRDLabel: "PRD"}, false},
		{"label + theme together still resyncs on the label", map[string]string{KeyPRDLabel: "Feature", KeyDefaultTheme: "mission"}, true},
		{"an empty update does not", map[string]string{}, false},
		// PRD #22 Decision 9: prdless keys never re-filter a board, so neither forces a resync.
		{"a prdless_enabled change does NOT force a resync", map[string]string{KeyPrdlessEnabled: "false"}, false},
		{"a prdless_label change does NOT force a resync", map[string]string{KeyPrdlessLabel: "NOSPEC"}, false},
		{"label + prdless together still resyncs on the label", map[string]string{KeyAutopilotLabel: "hands-off", KeyPrdlessLabel: "NOSPEC"}, true},
		// PRD #196 Decision 5: the list keys are deliberately NOT in the whitelist —
		// the sync fetch reads only the primary, so a resync on them is pointless and
		// adding them re-opens the ANDed-fetch eviction defect.
		{"a run_eligible_labels change does NOT force a resync", map[string]string{KeyRunEligibleLabels: "PRD,bug,security"}, false},
		{"a board_extra_labels change does NOT force a resync", map[string]string{KeyBoardExtraLabels: "documentation"}, false},
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
		KeyPRDLabel:          "PRD",
		KeyAutopilotLabel:    "autopilot",
		KeyRunEligibleLabels: "PRD", // PRD #196: eligible set must contain the primary
	}); err != nil {
		t.Errorf("distinct labels rejected: %v", err)
	}
}

// TestValidateMergedThreeWayDistinct covers PRD #22 Decision 7: prdless_label must
// be pairwise-distinct from prd_label and autopilot_label, the rejection names the
// offending key, and distinctness holds regardless of the toggle state (the merged
// map carries no prdless_enabled).
func TestValidateMergedThreeWayDistinct(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{
			KeyPRDLabel:          "PRD",
			KeyAutopilotLabel:    "autopilot",
			KeyPrdlessLabel:      "PRDLESS",
			KeyRunEligibleLabels: "PRD", // PRD #196: eligible set must contain the primary
		}
	}

	// prdless colliding with either other label is rejected, naming prdless_label.
	for _, collideWith := range []string{"PRD", "autopilot"} {
		m := base()
		m[KeyPrdlessLabel] = collideWith
		err := ValidateMerged(m)
		if err == nil || !strings.Contains(err.Error(), "prdless_label") {
			t.Errorf("prdless_label==%q: err = %v, want a rejection naming prdless_label", collideWith, err)
		}
	}

	// Three distinct labels pass.
	if err := ValidateMerged(base()); err != nil {
		t.Errorf("three distinct labels rejected: %v", err)
	}

	// Post-merge atomic + toggle-independent: an admin renaming prd_label onto the
	// (possibly disabled) prdless label collides on the merged set and is rejected —
	// keeping a later re-enable always safe. ValidateMerged never reads the toggle.
	m := base()
	m[KeyPRDLabel] = "PRDLESS"
	if err := ValidateMerged(m); err == nil {
		t.Error("prd_label renamed onto the prdless label must be rejected on the merged set")
	}
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBoardMembershipDefaults pins the PRD #196 compiled-in defaults on an empty
// table: the eligible set is the primary unioned with the stored default (PRD,bug
// with the primary first), extras are just bug, and the waiver is ON.
func TestBoardMembershipDefaults(t *testing.T) {
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.RunEligibleLabels(context.Background()); err != nil || !eq(got, []string{"PRD", "bug"}) {
		t.Fatalf("RunEligibleLabels default = %v, %v; want [PRD bug]", got, err)
	}
	if got, err := c.BoardExtraLabels(context.Background()); err != nil || !eq(got, []string{"bug"}) {
		t.Fatalf("BoardExtraLabels default = %v, %v; want [bug]", got, err)
	}
	if got, err := c.EligibleLabelWaivesPRDLink(context.Background()); err != nil || got != true {
		t.Fatalf("EligibleLabelWaivesPRDLink default = %v, %v; want true", got, err)
	}
}

// TestRunEligibleLabelsUnionsPrimary covers the fail-safe: the primary label is
// always in the eligible set, placed first and deduped, even when a hand-edited row
// dropped it or listed it out of position. The run gate must never make the primary
// non-runnable.
func TestRunEligibleLabelsUnionsPrimary(t *testing.T) {
	cases := []struct {
		name          string
		prd, eligible string
		want          []string
	}{
		{"row dropped the primary", "PRD", "bug,security", []string{"PRD", "bug", "security"}},
		{"primary listed but not first", "PRD", "bug,PRD,security", []string{"PRD", "bug", "security"}},
		{"row with only the primary yields it once", "PRD", "PRD", []string{"PRD"}},
		{"duplicates within the row are collapsed", "PRD", "bug,bug", []string{"PRD", "bug"}},
		{"custom primary is unioned in", "Feature", "bug", []string{"Feature", "bug"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(&fakeStore{rows: []store.AppSetting{
				row(KeyPRDLabel, tc.prd),
				row(KeyRunEligibleLabels, tc.eligible),
			}}, time.Minute)
			got, err := c.RunEligibleLabels(context.Background())
			if err != nil {
				t.Fatalf("RunEligibleLabels err = %v", err)
			}
			if !eq(got, tc.want) {
				t.Fatalf("RunEligibleLabels = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBoardExtraLabelsNoPrimaryUnion covers the orthogonality (Decision 2): extras
// carry NO primary union, so an extra is removable and membership is primary ∪
// extras rather than a set that pins the primary twice.
func TestBoardExtraLabelsNoPrimaryUnion(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{
		row(KeyPRDLabel, "PRD"),
		row(KeyBoardExtraLabels, "bug,documentation"),
	}}, time.Minute)
	got, err := c.BoardExtraLabels(context.Background())
	if err != nil || !eq(got, []string{"bug", "documentation"}) {
		t.Fatalf("BoardExtraLabels = %v, %v; want [bug documentation] with no primary", got, err)
	}
	// The result is always non-nil (so it JSON-encodes as [] not null): a
	// present-but-empty value falls back to the compiled-in default like every other
	// setting (empty → default), and parseLabelList returns a non-nil slice.
	if got == nil {
		t.Fatal("BoardExtraLabels returned a nil slice")
	}
}

// TestEligibleLabelWaivesPRDLinkJunkTolerance covers the strict "true"/"false"
// parse defaulting ON: a malformed value never silently flips the default-on gate
// off.
func TestEligibleLabelWaivesPRDLinkJunkTolerance(t *testing.T) {
	for _, tc := range []struct {
		stored string
		want   bool
	}{
		{"true", true},
		{"false", false},
		{"", true},       // empty → default true
		{"banana", true}, // junk → default, NOT false
		{"TRUE", true},   // non-canonical → default
		{"1", true},
	} {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeyEligibleLabelWaivesPRDLink, tc.stored)}}, time.Minute)
		if got, _ := c.EligibleLabelWaivesPRDLink(context.Background()); got != tc.want {
			t.Errorf("EligibleLabelWaivesPRDLink(stored=%q) = %v, want %v", tc.stored, got, tc.want)
		}
	}
}

func TestParseLabelList(t *testing.T) {
	// Order preserved, tokens trimmed, empties dropped, no dedup (dedup is a
	// ValidateMerged concern), and the result is always non-nil.
	if got := parseLabelList(" PRD , bug ,, PRD "); !eq(got, []string{"PRD", "bug", "PRD"}) {
		t.Errorf("parseLabelList = %v, want [PRD bug PRD] (order, trim, no dedup)", got)
	}
	if got := parseLabelList(""); got == nil || len(got) != 0 {
		t.Errorf("parseLabelList(\"\") = %v, want non-nil empty", got)
	}
}

func TestValidateLabelList(t *testing.T) {
	// A valid comma list passes; empty passes (no extras); a too-long token fails.
	if err := validateLabelList("PRD,bug,security"); err != nil {
		t.Errorf("valid list rejected: %v", err)
	}
	if err := validateLabelList(""); err != nil {
		t.Errorf("empty list rejected: %v", err)
	}
	if err := validateLabelList("PRD," + strings.Repeat("x", maxLabelLen+1)); err == nil {
		t.Error("a too-long token should be rejected")
	}
}

// TestValidateMergedListRules covers the PRD #196 cross-key + structural checks:
// the primary is not removable from the eligible set, neither list may duplicate an
// entry, neither may exceed the count cap, and neither may carry a workflow marker
// (autopilot / prdless).
func TestValidateMergedListRules(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{
			KeyPRDLabel:          "PRD",
			KeyAutopilotLabel:    "autopilot",
			KeyPrdlessLabel:      "PRDLESS",
			KeyRunEligibleLabels: "PRD,bug",
			KeyBoardExtraLabels:  "bug",
		}
	}

	// A well-formed default set passes.
	if err := ValidateMerged(base()); err != nil {
		t.Fatalf("default lists rejected: %v", err)
	}

	// An eligible set omitting the primary is ACCEPTED, not rejected: ValidateMerged
	// unions the primary in (matching the RunEligibleLabels accessor's fail-safe), so
	// the primary is always eligible without a write-time error that would wedge
	// unrelated PUTs. The AdminSettings UI pins the primary so a normal save carries it.
	m := base()
	m[KeyRunEligibleLabels] = "bug,security"
	if err := ValidateMerged(m); err != nil {
		t.Errorf("eligible set omitting the primary should be accepted (primary is unioned in): %v", err)
	}

	// Regression: on an instance that renamed prd_label, the compiled-in default
	// run_eligible_labels ("PRD,bug") does not contain the renamed primary. A hard
	// "must contain the primary" check here rejected EVERY settings PUT on such an
	// instance (it re-validates the whole merged state). The union must make this pass.
	m = base()
	m[KeyPRDLabel] = "spec"
	m[KeyRunEligibleLabels] = "PRD,bug" // the default, which lacks the renamed primary
	if err := ValidateMerged(m); err != nil {
		t.Errorf("renamed-primary instance with default eligible set should not wedge settings PUT: %v", err)
	}

	// Duplicate entries are rejected, in either list.
	m = base()
	m[KeyRunEligibleLabels] = "PRD,bug,bug"
	if err := ValidateMerged(m); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate eligible entry: err = %v, want a duplicate rejection", err)
	}
	m = base()
	m[KeyBoardExtraLabels] = "bug,bug"
	if err := ValidateMerged(m); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate extra entry: err = %v, want a duplicate rejection", err)
	}

	// The count cap bounds each list.
	many := make([]string, 0, maxLabelListLen+2)
	many = append(many, "PRD")
	for i := 0; i < maxLabelListLen+1; i++ {
		many = append(many, "lbl"+strconv.Itoa(i))
	}
	m = base()
	m[KeyRunEligibleLabels] = strings.Join(many, ",")
	if err := ValidateMerged(m); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("over-cap eligible list: err = %v, want a count-cap rejection", err)
	}

	// A workflow marker (autopilot / prdless) is never membership/eligibility content.
	for _, marker := range []string{"autopilot", "PRDLESS"} {
		m = base()
		m[KeyRunEligibleLabels] = "PRD," + marker
		if err := ValidateMerged(m); err == nil {
			t.Errorf("eligible set containing the %q marker should be rejected", marker)
		}
		m = base()
		m[KeyBoardExtraLabels] = "bug," + marker
		if err := ValidateMerged(m); err == nil {
			t.Errorf("extras containing the %q marker should be rejected", marker)
		}
	}
}
