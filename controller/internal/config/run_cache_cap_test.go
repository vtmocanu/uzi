package config

import (
	"os"
	"testing"
)

func TestWorkerRunCacheCapRelayVerbatim(t *testing.T) {
	const (
		maxKey  = "UZI_WORKER_RUN_CACHE_CAP_MAX_BYTES"
		fracKey = "UZI_WORKER_RUN_CACHE_CAP_FRACTION"
	)
	// set: a nil pointer means the key is unset (not just empty).
	str := func(s string) *string { return &s }
	cases := []struct {
		name      string
		max, frac *string
	}{
		{"off-and-quarter", str(" OFF "), str("0.25")},
		{"zero-and-off", str("0"), str(" off ")},
		{"junk-and-junk", str("not-a-number"), str("x")},
		{"twenty-gib-and-half", str("21474836480"), str("0.5")},
		{"max-only", str("21474836480"), nil},
		{"fraction-only", nil, str("0.25")},
		{"both-empty", str(""), str("")},
		{"both-unset", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setWorkerEnv(t)
			t.Setenv("UZI_API_URL", "https://uzi.example.com")
			t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "controller-token"))
			for k, v := range map[string]*string{maxKey: tc.max, fracKey: tc.frac} {
				if v == nil {
					t.Setenv(k, "")
					_ = os.Unsetenv(k)
				} else {
					t.Setenv(k, *v)
				}
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			wantMax, wantFrac := "", ""
			if tc.max != nil {
				wantMax = *tc.max
			}
			if tc.frac != nil {
				wantFrac = *tc.frac
			}
			if cfg.WorkerRunCacheCapMaxBytes != wantMax {
				t.Errorf("WorkerRunCacheCapMaxBytes = %q, want %q", cfg.WorkerRunCacheCapMaxBytes, wantMax)
			}
			if cfg.WorkerRunCacheCapFraction != wantFrac {
				t.Errorf("WorkerRunCacheCapFraction = %q, want %q", cfg.WorkerRunCacheCapFraction, wantFrac)
			}
		})
	}
}
