package config

import (
	"testing"
	"time"
)

func TestLoadDurationKnobBoundaries(t *testing.T) {
	setWorkerEnv(t)
	t.Setenv("UZI_API_URL", "https://uzi.example.com")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "t"))

	knobs := []struct {
		env      string
		seed     string
		fallback time.Duration
	}{
		{"CONTROLLER_POLL_INTERVAL", "23s", 10 * time.Second},
		{"CONTROLLER_HTTP_TIMEOUT", "7s", 15 * time.Second},
		{"UZI_WORKER_DRAIN_DEADLINE", "2h", 24 * time.Hour},
		{"UZI_WORKER_DISK_RECYCLE_COOLDOWN", "45m", time.Hour},
	}
	seeded := [4]time.Duration{23 * time.Second, 7 * time.Second, 2 * time.Hour, 45 * time.Minute}
	for _, knob := range knobs {
		t.Setenv(knob.env, knob.seed)
	}

	for i, knob := range knobs {
		t.Run(knob.env, func(t *testing.T) {
			// Catch lost trimming or fractional precision, a minimum-duration clamp,
			// acceptance of non-positive values, and mishandled parse overflow.
			cases := []struct {
				name string
				raw  string
				want time.Duration
			}{
				{"padded composite", " \t1m2.5s\n", 62500 * time.Millisecond},
				{"smallest positive", "1ns", time.Nanosecond},
				{"zero", "0s", knob.fallback},
				{"negative", "-1ns", knob.fallback},
				{"whitespace only", " \t\n", knob.fallback},
				{"overflow", "9223372036854775808ns", knob.fallback},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					t.Setenv(knob.env, tc.raw)
					cfg, err := Load()
					if err != nil {
						t.Fatalf("Load with %s=%q: %v", knob.env, tc.raw, err)
					}
					// Exact siblings expose crossed env bindings and fallback leakage
					// that checking only the varied field would miss.
					want := seeded
					want[i] = tc.want
					got := [4]time.Duration{
						cfg.PollInterval,
						cfg.HTTPTimeout,
						cfg.DrainDeadline,
						cfg.WorkerDiskRecycleCooldown,
					}
					if got != want {
						t.Fatalf("%s=%q: [PollInterval HTTPTimeout DrainDeadline WorkerDiskRecycleCooldown] = %v, want %v", knob.env, tc.raw, got, want)
					}
				})
			}
		})
	}
}
