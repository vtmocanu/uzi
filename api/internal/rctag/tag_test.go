package rctag

import (
	"strings"
	"testing"
)

func TestIsPublishedTag(t *testing.T) {
	for _, tc := range []struct {
		tag  string
		want bool
	}{
		// Former releasecheck predicate cases.
		{"v1.2.3-rc.1", true},
		{"v1.2.3-rc.10", true},
		{"1.2.3-rc.1", false},
		{"v1.2.3-rc.01", false},
		{"v1.2.3-rc.1.extra", false},
		{"v1.2.3-rc.1+build", false},
		{"v1.2.3-beta.1", false},
		{"v1.2.3-beta-rc.1", false},
		{"v1.2.3-rc.1-rc.2", false},
		{"v1.2.3", false},
		{"v1.2.3-rc.x", false},
		// Former TUI predicate cases.
		{"v0.85.0-rc.1", true},
		{"v0.85.0+build-rc.1", false},
		{"v0.85.0-rc.1+build", false},
		{"v0.85.0-rc.01", false},
		{"v00.85.0-rc.1", false},
		{"0.85.0-rc.1", false},
		{"v0.85.0-rc.0", false},
		{"v0.85.0-rc", false},
		{"v0.85.0-rc.1.2", false},
		{"v0.85.0-beta.1", false},
		{"v0.85.0", false},
		{"v0.85-rc.1", false},
		{"v0.85.0-rc.1+meta", false},
		{"v0.085.0-rc.1", false},
		{"v0.85.00-rc.1", false},
		{"v0-rc.1", false},
		{"v0.85.0.0-rc.1", false},
		{"v0.85.0-rc.12", true},
		{"v0.85.0-rc." + strings.Repeat("9", 256), true},
		{"v0.85.0-rc.١", false},
		{" v0.85.0-rc.1", false},
		{"v0.85.0-rc.1 ", false},
		{"", false},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			if got := IsPublishedTag(tc.tag); got != tc.want {
				t.Errorf("IsPublishedTag(%q) = %v, want %v", tc.tag, got, tc.want)
			}
		})
	}
}

func TestIsStampedVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"v0.85.0-rc.1", true},
		{"v0.85.0-rc.1+meta", true},
		{"v0.85.0-rc.12", true},
		{"v0.85.0-rc.12+build.001-a", true},
		{"v0.85.0-rc." + strings.Repeat("9", 256), true},
		{"v0.85.0-rc." + strings.Repeat("9", 256) + "+meta", true},
		{"v0.85.0-rc.0", false},
		{"v0.85.0-rc.0+meta", false},
		{"v0.85.0-rc.01", false},
		{"v0.85.0-rc.01+meta", false},
		{"v0.85.0-rc.1+", false},
		{"v0.85.0-rc.1+bad..meta", false},
		{"v0.85.0-rc.1+bad_meta", false},
		{"v0.85.0-rc.1+meta+extra", false},
		{"v0.85.0-rc.1.2", false},
		{"v0.85.0-rc.1.2+meta", false},
		{"v0.85.0-rc", false},
		{"v0.85.0-beta.1", false},
		{"v0.85.0-beta-rc.1", false},
		{"v0.85.0-beta-rc.1+meta", false},
		{"v0.85.0-rc.1-rc.2", false},
		{"v0.85.0+meta", false},
		{"v0.85.0", false},
		{"0.85.0-rc.1", false},
		{"0.85.0-rc.1+meta", false},
		{"v0.85-rc.1", false},
		{"v00.85.0-rc.1", false},
		{"v0.085.0-rc.1", false},
		{"v0.85.00-rc.1", false},
		{"v0.85.0.0-rc.1", false},
		{"v0.85.0-rc.x", false},
		{"v0.85.0-rc.١", false},
		{" v0.85.0-rc.1+meta", false},
		{"v0.85.0-rc.1+meta ", false},
		{"", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			if got := IsStampedVersion(tc.version); got != tc.want {
				t.Errorf("IsStampedVersion(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}
