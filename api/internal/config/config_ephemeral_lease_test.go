package config

import (
	"strings"
	"testing"
	"time"
)

// TestLoadEphemeralLease pins the strict UZI_EPHEMERAL_LEASE parser (PRD #2006): unset or
// empty defaults to 2h, "0" disables, in-range values are kept, and a malformed, negative or
// over-cap value makes Load fail so the api refuses to boot.
func TestLoadEphemeralLease(t *testing.T) {
	cases := []struct {
		name    string
		set     bool
		value   string
		want    time.Duration
		wantErr string
	}{
		{name: "unset defaults to 2h", want: 2 * time.Hour},
		{name: "empty defaults to 2h", set: true, value: "", want: 2 * time.Hour},
		{name: "zero disables", set: true, value: "0", want: 0},
		{name: "30m", set: true, value: "30m", want: 30 * time.Minute},
		{name: "2h is the cap and accepted", set: true, value: "2h", want: 2 * time.Hour},
		{name: "2h1m is over the cap", set: true, value: "2h1m", wantErr: "exceeds the maximum"},
		{name: "negative", set: true, value: "-1m", wantErr: "negative"},
		{name: "malformed", set: true, value: "abc", wantErr: "not a valid duration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			autoselectEnv(t)
			if tc.set {
				t.Setenv("UZI_EPHEMERAL_LEASE", tc.value)
			}
			cfg, err := Load()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Load() succeeded with lease %q (EphemeralLease=%s); want an error containing %q", tc.value, cfg.EphemeralLease, tc.wantErr)
				}
				if !strings.Contains(err.Error(), "UZI_EPHEMERAL_LEASE") || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load() error = %v; want it to name UZI_EPHEMERAL_LEASE and contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if cfg.EphemeralLease != tc.want {
				t.Fatalf("EphemeralLease = %s, want %s", cfg.EphemeralLease, tc.want)
			}
		})
	}
}
