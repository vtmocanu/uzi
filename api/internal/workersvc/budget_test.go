package workersvc

import (
	"testing"
	"time"
)

// TestNewDerivesUnwiredRunWallCeiling pins #2279's fail-safe: a Params without
// RunWallCeiling must not freeze 0-second walls; New derives config.Load's unset default.
func TestNewDerivesUnwiredRunWallCeiling(t *testing.T) {
	for _, tc := range []struct {
		name          string
		base, ceiling time.Duration
		want          time.Duration
	}{
		{"zero base", 0, 0, 24 * time.Hour},
		{"default base", 6 * time.Hour, 0, 24 * time.Hour},
		{"long base", 30 * time.Hour, 0, 30 * time.Hour},
		{"huge base capped", 100 * time.Hour, 0, 72 * time.Hour},
		{"wired value kept", 6 * time.Hour, 10 * time.Hour, 10 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(nil, nil, Params{RunTimeout: tc.base, RunWallCeiling: tc.ceiling})
			if s.p.RunWallCeiling != tc.want {
				t.Fatalf("RunWallCeiling=%v want %v", s.p.RunWallCeiling, tc.want)
			}
		})
	}
}
