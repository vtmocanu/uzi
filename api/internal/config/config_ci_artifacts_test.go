package config

import "testing"

func TestCIArtifactLimits(t *testing.T) {
	for _, tc := range []struct {
		value string
		files int
		bytes int64
	}{
		{"", 50, 16 << 20}, {"0", 50, 16 << 20}, {"-1", 50, 16 << 20}, {"bad", 50, 16 << 20}, {"1", 1, 1}, {"999999999", 50, 16 << 20},
	} {
		t.Run(tc.value, func(t *testing.T) {
			autoselectEnv(t)
			t.Setenv("UZI_CI_ARTIFACT_MAX_FILES", tc.value)
			t.Setenv("UZI_CI_ARTIFACT_MAX_BYTES", tc.value)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CIArtifactMaxFiles != tc.files || cfg.CIArtifactMaxBytes != tc.bytes {
				t.Fatalf("limits %d/%d", cfg.CIArtifactMaxFiles, cfg.CIArtifactMaxBytes)
			}
		})
	}
}
