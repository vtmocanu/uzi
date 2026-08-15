package toolseed

import (
	"bytes"
	"os"
	"testing"
)

// TestSeedCopyMatchesManifest is the Decision 5 golden: the embedded copy under
// this package MUST be byte-identical to agent/devbox-global/devbox.json, the file
// the worker images actually install. go:embed cannot reach the manifest across
// the package boundary, so a copy is shipped here; this test is what stops the
// copy drifting silently. Go tests run with cwd = the package dir, so the relative
// path reaches the repo-root agent manifest.
func TestSeedCopyMatchesManifest(t *testing.T) {
	const manifestPath = "../../../agent/devbox-global/devbox.json"
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read source manifest %s: %v", manifestPath, err)
	}
	if !bytes.Equal(manifest, seedManifest) {
		t.Fatalf("embedded devbox.json has drifted from %s; re-copy it:\n"+
			"\tcp agent/devbox-global/devbox.json api/internal/toolseed/devbox.json",
			manifestPath)
	}
}

// TestSeededAllowlistCoveredBySeed asserts every post-M2 seeded allowlist name is
// Covered (SC3(ii)). The set is derived from the migration 00046 seed as swapped
// by migration 00124 (terraform → opentofu): {kubectl, opentofu, jq, yq, ripgrep,
// fd, go, nodejs, python3}. kubectl and nodejs are covered via seedExceptions
// (allowlisted-but-unbaked, Decision 4); the rest are baked.
func TestSeededAllowlistCoveredBySeed(t *testing.T) {
	covered := []string{
		"kubectl", "opentofu", "jq", "yq", "ripgrep",
		"fd", "go", "nodejs", "python3",
	}
	for _, name := range covered {
		if !Covered(name) {
			t.Errorf("Covered(%q) = false, want true (seeded allowlist name must be covered)", name)
		}
	}

	// NOT-covered examples: a package neither baked nor an exception, and two
	// package-vs-name traps.
	notCovered := []string{
		"ruby",      // never baked (tier-2 only), not an exception
		"helm",      // baked as kubernetes-helm, not `helm` (binary name, not attr)
		"terraform", // swapped off the allowlist by 00124, not baked (unfree)
	}
	for _, name := range notCovered {
		if Covered(name) {
			t.Errorf("Covered(%q) = true, want false", name)
		}
	}

	// @version is split off before the coverage check.
	if !Covered("python3@3.12") {
		t.Errorf(`Covered("python3@3.12") = false, want true (version must be split off)`)
	}
	if Covered("ruby@3.3") {
		t.Errorf(`Covered("ruby@3.3") = true, want false`)
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"jq.bin":              "jq",                  // output selector stripped
		"openssl.bin":         "openssl",             // output selector stripped
		"file.out":            "file",                // output selector stripped
		"shellcheck.bin":      "shellcheck",          // output selector stripped
		"yq-go":               "yq",                  // alias applied
		"python3Packages.pip": "python3Packages.pip", // .pip is not an output name
		"ripgrep":             "ripgrep",             // identity
		"opentofu":            "opentofu",            // identity
		"go-task":             "go-task",             // NOT aliased to `task`
		"kubernetes-helm":     "kubernetes-helm",     // NOT aliased to `helm`
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
