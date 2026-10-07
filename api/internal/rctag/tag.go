// Package rctag validates release-candidate tags and binary version stamps.
package rctag

import (
	"strings"

	"golang.org/x/mod/semver"
)

// IsPublishedTag reports whether tag is a canonical vMAJOR.MINOR.PATCH-rc.N
// release tag, with positive N and no build metadata.
func IsPublishedTag(tag string) bool {
	return parse(tag, false)
}

// IsStampedVersion accepts the same RC channel as IsPublishedTag, permitting
// valid SemVer build metadata on a binary stamp. It does not normalize input.
func IsStampedVersion(version string) bool {
	return parse(version, true)
}

func parse(version string, allowMetadata bool) bool {
	if !strings.HasPrefix(version, "v") || !semver.IsValid(version) {
		return false
	}
	if !allowMetadata && semver.Build(version) != "" {
		return false
	}
	tag, _, _ := strings.Cut(version, "+")
	base, n, ok := strings.Cut(tag, "-rc.")
	if !ok || semver.Canonical(base) != base || semver.Prerelease(base) != "" || n == "" || n[0] == '0' {
		return false
	}
	// Scan every byte, without an integer conversion or a numeric size limit.
	for i := 0; i < len(n); i++ {
		if n[i] < '0' || n[i] > '9' {
			return false
		}
	}
	return true
}
