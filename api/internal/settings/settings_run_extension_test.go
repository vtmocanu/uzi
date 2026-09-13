package settings

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestValidateRunExtensionCapSeconds pins the write-time bounds for the per-run extension
// cap (PRD #1189 D3): the value must be 0 (extending disabled) or within [3600, 604800]
// (1h to 7d). The routing goes through the public Validate(key, value), so this also proves
// KeyRunExtensionCapSeconds is wired to validateExtensionCapSeconds and not something else.
func TestValidateRunExtensionCapSeconds(t *testing.T) {
	accept := []string{"0", "3600", "604800", "57600", " 57600 "}
	for _, v := range accept {
		if err := Validate(KeyRunExtensionCapSeconds, v); err != nil {
			t.Errorf("Validate(run_extension_cap_seconds, %q) = %v, want nil", v, err)
		}
	}
	// Reject: below the 1h floor (but non-zero), above the 7d ceiling, negatives, and
	// non-integers including the empty string.
	reject := []string{"3599", "604801", "-1", "abc", "", "3600.5", "1e3", "1"}
	for _, v := range reject {
		if err := Validate(KeyRunExtensionCapSeconds, v); err == nil {
			t.Errorf("Validate(run_extension_cap_seconds, %q) = nil, want a rejection", v)
		}
	}
	// The out-of-range message is exact (the admin card mirrors it verbatim).
	if err := Validate(KeyRunExtensionCapSeconds, "3599"); err == nil ||
		err.Error() != "must be 0 (disabled) or between 3600 and 604800 seconds" {
		t.Errorf("Validate(run_extension_cap_seconds, 3599) = %v, want the exact out-of-range message", err)
	}
}

// TestRunExtensionCapKnownAndDefault pins that the key is Known (so the admin write path does
// not 400 it) and carries the documented default of 57600 (16h) in the Defaults map, which is
// what surfaces it in GET /api/admin/settings with no per-key handler.
func TestRunExtensionCapKnownAndDefault(t *testing.T) {
	if !Known(KeyRunExtensionCapSeconds) {
		t.Errorf("Known(%q) = false, want true", KeyRunExtensionCapSeconds)
	}
	if got, ok := Defaults[KeyRunExtensionCapSeconds]; !ok {
		t.Errorf("Defaults[%q] missing — All/AdminView would not surface it", KeyRunExtensionCapSeconds)
	} else if got != "57600" {
		t.Errorf("Defaults[%q] = %q, want \"57600\" (16h)", KeyRunExtensionCapSeconds, got)
	}
	if DefaultRunExtensionCapSeconds != "57600" {
		t.Errorf("DefaultRunExtensionCapSeconds = %q, want \"57600\"", DefaultRunExtensionCapSeconds)
	}
}

// TestRunExtensionCapAccessor pins the *Cache accessor: with no stored row it falls back to
// the compiled default 57600, and a stored row is read back verbatim (including 0, which
// disables extending). The fakeStore serves rows from memory, so this needs no live DB.
func TestRunExtensionCapAccessor(t *testing.T) {
	ctx := context.Background()

	def := New(&fakeStore{}, time.Minute)
	if got, err := def.RunExtensionCapSeconds(ctx); err != nil || got != 57600 {
		t.Errorf("RunExtensionCapSeconds default = %d (err %v), want 57600", got, err)
	}

	set := New(&fakeStore{rows: []store.AppSetting{row(KeyRunExtensionCapSeconds, "7200")}}, time.Minute)
	if got, err := set.RunExtensionCapSeconds(ctx); err != nil || got != 7200 {
		t.Errorf("RunExtensionCapSeconds stored = %d (err %v), want 7200", got, err)
	}

	off := New(&fakeStore{rows: []store.AppSetting{row(KeyRunExtensionCapSeconds, "0")}}, time.Minute)
	if got, err := off.RunExtensionCapSeconds(ctx); err != nil || got != 0 {
		t.Errorf("RunExtensionCapSeconds disabled = %d (err %v), want 0", got, err)
	}
}
