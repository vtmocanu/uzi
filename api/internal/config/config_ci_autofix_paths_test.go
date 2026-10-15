package config

import (
	"slices"
	"testing"
)

// The auto CI-fix guard's protected set always carries the floor: an operator list adds
// paths, it can never drop a forge's CI-config location.
func TestCIAutofixConfigPathsAlwaysCarryTheFloor(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		extra     []string
	}{
		{"unset", "", nil},
		{"a GitLab-only operator list", ".gitlab-ci.yml,.gitlab/**", nil},
		{"an added path", "deploy/**", []string{"deploy/**"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			autoselectEnv(t)
			t.Setenv("CI_AUTOFIX_CONFIG_PATHS", tc.env)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			want := append(slices.Clone(CIAutofixConfigFloor), tc.extra...)
			if !slices.Equal(cfg.CIAutofixConfigPaths, want) {
				t.Fatalf("CIAutofixConfigPaths = %q, want %q", cfg.CIAutofixConfigPaths, want)
			}
			for _, dir := range []string{".github/workflows/**", ".forgejo/workflows/**", ".gitea/workflows/**"} {
				if !slices.Contains(cfg.CIAutofixConfigPaths, dir) {
					t.Fatalf("%s missing from %q", dir, cfg.CIAutofixConfigPaths)
				}
			}
		})
	}
}
