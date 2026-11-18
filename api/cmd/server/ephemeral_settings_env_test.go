package main

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/settings"
)

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func TestEphemeralSettingsEnv(t *testing.T) {
	cfg := &config.Config{
		EphemeralMaxPerUser:  2,
		EphemeralDefaultSize: "m",
		EphemeralLease:       2 * time.Hour,
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		cfg  config.Config
		want map[string]string
	}{
		{name: "unset", env: map[string]string{}, cfg: *cfg, want: map[string]string{}},
		{
			name: "empty and blank are absent",
			env: map[string]string{
				"UZI_EPHEMERAL_MAX_PER_USER": "",
				"UZI_EPHEMERAL_DEFAULT_SIZE": "   ",
				"UZI_EPHEMERAL_LEASE":        "\t",
			},
			cfg:  *cfg,
			want: map[string]string{},
		},
		{
			name: "lease is canonical",
			env:  map[string]string{"UZI_EPHEMERAL_LEASE": "30m"},
			cfg:  config.Config{EphemeralMaxPerUser: 2, EphemeralDefaultSize: "m", EphemeralLease: 30 * time.Minute},
			want: map[string]string{settings.KeyEphemeralLease: "30m"},
		},
		{
			name: "lease 2h",
			env:  map[string]string{"UZI_EPHEMERAL_LEASE": "2h"},
			cfg:  *cfg,
			want: map[string]string{settings.KeyEphemeralLease: "2h"},
		},
		{
			name: "malformed cap shows the value config ran with",
			env:  map[string]string{"UZI_EPHEMERAL_MAX_PER_USER": "abc"},
			cfg:  *cfg,
			want: map[string]string{settings.KeyEphemeralMaxPerUser: "2"},
		},
		{
			name: "cap above the admin bound is honoured",
			env:  map[string]string{"UZI_EPHEMERAL_MAX_PER_USER": "25"},
			cfg:  config.Config{EphemeralMaxPerUser: 25, EphemeralDefaultSize: "m", EphemeralLease: 2 * time.Hour},
			want: map[string]string{settings.KeyEphemeralMaxPerUser: "25"},
		},
		{
			name: "size",
			env:  map[string]string{"UZI_EPHEMERAL_DEFAULT_SIZE": "s"},
			cfg:  config.Config{EphemeralMaxPerUser: 2, EphemeralDefaultSize: "s", EphemeralLease: 2 * time.Hour},
			want: map[string]string{settings.KeyEphemeralDefaultSize: "s"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.cfg
			got := ephemeralSettingsEnv(lookupFrom(tc.env), &c)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("got[%s] = %q, want %q (full: %v)", k, got[k], v, got)
				}
			}
		})
	}
}
