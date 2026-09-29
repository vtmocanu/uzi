package settings

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestFetchCapsKnownAndDefaults pins the four PRD #1906 keys as Known (so the admin PUT does
// not 400 them) with the Open question 1 defaults in Defaults (which is what surfaces them in
// GET /api/admin/settings with no per-key handler).
func TestFetchCapsKnownAndDefaults(t *testing.T) {
	want := map[string]string{
		"fetch_max_file_bytes":         "26214400",  // 25 MiB
		"fetch_max_run_bytes":          "209715200", // 200 MiB
		"fetch_max_run_files":          "100",
		"fetch_max_concurrent_per_run": "4",
	}
	for key, def := range want {
		if !Known(key) {
			t.Errorf("Known(%q) = false, want true", key)
		}
		if got, ok := Defaults[key]; !ok || got != def {
			t.Errorf("Defaults[%q] = %q (present %v), want %q", key, got, ok, def)
		}
		if IsSecret(key) {
			t.Errorf("IsSecret(%q) = true, want false", key)
		}
	}
}

// TestValidateFetchCaps pins the write-time bounds through the public Validate, which also
// proves each key is routed to validateFetchCap and not to the label default branch.
func TestValidateFetchCaps(t *testing.T) {
	cases := []struct {
		key    string
		accept []string
		reject []string
	}{
		{KeyFetchMaxFileBytes, []string{"1", "26214400", "1073741824", " 1024 "},
			[]string{"0", "-1", "1073741825", "25MiB", "", "1e6", "1.5"}},
		{KeyFetchMaxRunBytes, []string{"1", "209715200", "10737418240"},
			[]string{"0", "10737418241", "abc"}},
		{KeyFetchMaxRunFiles, []string{"1", "100", "10000"},
			[]string{"0", "10001", "-5"}},
		{KeyFetchMaxConcurrentRun, []string{"1", "4", "32"},
			[]string{"0", "33", "four"}},
	}
	for _, c := range cases {
		for _, v := range c.accept {
			if err := Validate(c.key, v); err != nil {
				t.Errorf("Validate(%s, %q) = %v, want nil", c.key, v, err)
			}
		}
		for _, v := range c.reject {
			if err := Validate(c.key, v); err == nil {
				t.Errorf("Validate(%s, %q) = nil, want a rejection", c.key, v)
			}
		}
	}
	if err := Validate(KeyFetchMaxConcurrentRun, "33"); err == nil || err.Error() != "must be between 1 and 32" {
		t.Errorf("Validate(concurrent, 33) = %v, want the exact range message", err)
	}
	if err := Validate(KeyFetchMaxRunFiles, "x"); err == nil || err.Error() != "must be a whole number" {
		t.Errorf("Validate(files, x) = %v, want the exact parse message", err)
	}
}

// TestFetchCapsAccessor pins the accessor: defaults with no rows, stored values read back,
// and a stored value outside its bounds (written around the validator) reading as the
// default, so a bad row can never turn a cap off.
func TestFetchCapsAccessor(t *testing.T) {
	ctx := context.Background()

	def, err := New(&fakeStore{}, time.Minute).FetchCaps(ctx)
	if err != nil {
		t.Fatalf("FetchCaps default: %v", err)
	}
	if want := (FetchCaps{MaxFileBytes: 25 << 20, MaxRunBytes: 200 << 20, MaxRunFiles: 100, MaxConcurrentPerRun: 4}); def != want {
		t.Fatalf("FetchCaps default = %+v, want %+v", def, want)
	}

	set, err := New(&fakeStore{rows: []store.AppSetting{
		row(KeyFetchMaxFileBytes, "1048576"),
		row(KeyFetchMaxRunBytes, "5368709120"), // 5 GiB: above int32, must not truncate
		row(KeyFetchMaxRunFiles, "7"),
		row(KeyFetchMaxConcurrentRun, "2"),
	}}, time.Minute).FetchCaps(ctx)
	if err != nil {
		t.Fatalf("FetchCaps stored: %v", err)
	}
	if want := (FetchCaps{MaxFileBytes: 1 << 20, MaxRunBytes: 5 << 30, MaxRunFiles: 7, MaxConcurrentPerRun: 2}); set != want {
		t.Fatalf("FetchCaps stored = %+v, want %+v", set, want)
	}

	bad, err := New(&fakeStore{rows: []store.AppSetting{
		row(KeyFetchMaxFileBytes, "0"),
		row(KeyFetchMaxRunBytes, "-1"),
		row(KeyFetchMaxRunFiles, "junk"),
		row(KeyFetchMaxConcurrentRun, "1000"),
	}}, time.Minute).FetchCaps(ctx)
	if err != nil {
		t.Fatalf("FetchCaps bad rows: %v", err)
	}
	if bad != def {
		t.Fatalf("FetchCaps with out-of-bounds rows = %+v, want the defaults %+v", bad, def)
	}
}
