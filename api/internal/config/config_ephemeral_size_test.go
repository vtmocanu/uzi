package config

import (
	"os"
	"testing"
)

func TestLoadEphemeralDefaultSize(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		unset bool
		want  string
	}{
		{name: "unset", unset: true, want: "m"},
		{name: "empty", want: "m"},
		{name: "invalid", value: "unknown", want: "m"},
		{name: "whitespace-only value uses the default", value: "  ", want: "m"},
		{name: "explicit small", value: "s", want: "s"},
		{name: "explicit medium", value: "m", want: "m"},
		{name: "trimmed large", value: " l ", want: "l"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			autoselectEnv(t)
			t.Setenv("UZI_EPHEMERAL_DEFAULT_SIZE", tc.value)
			if tc.unset {
				if err := os.Unsetenv("UZI_EPHEMERAL_DEFAULT_SIZE"); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if cfg.EphemeralDefaultSize != tc.want {
				t.Fatalf("EphemeralDefaultSize = %q, want %q", cfg.EphemeralDefaultSize, tc.want)
			}
		})
	}
}
