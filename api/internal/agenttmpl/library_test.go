package agenttmpl

// PRD #85 M4: builtin ↔ library drift check.
//
// This test compares the version stamp each shipped builtin carries against a
// vendored manifest distilled from the upstream skills library
// (github.com/vtmocanu/skills roles.yaml at the pinned sha in the manifest). It
// fails ONLY when a stamp is BEHIND the manifest (the body needs a port), or when
// a builtin is stamped at a version the manifest does not have (unknown/ahead — a
// manifest refresh that landed with no matching body change). A deliberate
// manifest refresh with no accompanying body port is exactly what this reddens on,
// which is the point: it forces the port or an explicit re-stamp.
//
// It deliberately does NOT assert roster completeness against the FULL upstream
// library. Upstream ships 14 roles; uzi ships 11 by design, omitting `release`,
// `tui-ux`, and `skill-reviewer`. The manifest is the only roster this test knows,
// and it lists exactly those 11 — so the check never reddens on the three omitted
// roles. `lead` is a uzi-only role, unstamped (Version 0), and is excluded from
// every comparison here — it is never checked.

import (
	_ "embed"
	"encoding/json"
	"testing"
)

//go:embed library/manifest.json
var libraryManifestJSON []byte

// libraryManifest is the parsed shape of library/manifest.json. Embedding and
// parsing it in a _test.go file keeps the manifest off the production surface
// (Decision 8): nothing in the shipped binary depends on it.
type libraryManifest struct {
	UpstreamRepo string         `json:"upstream_repo"`
	UpstreamSHA  string         `json:"upstream_sha"`
	Path         string         `json:"path"`
	Synced       string         `json:"synced"`
	Roles        map[string]int `json:"roles"`
}

func TestBuiltinLibraryDrift(t *testing.T) {
	var m libraryManifest
	if err := json.Unmarshal(libraryManifestJSON, &m); err != nil {
		t.Fatalf("parse library/manifest.json: %v", err)
	}

	// (4) A malformed/empty manifest must not pass silently.
	if m.UpstreamSHA == "" {
		t.Fatalf("manifest upstream_sha is empty — malformed manifest")
	}
	if len(m.Roles) == 0 {
		t.Fatalf("manifest roles map is empty — malformed manifest")
	}

	// stamped = builtins whose Version > 0. `lead` (Version 0) is excluded and
	// never checked.
	stamped := make(map[string]int)
	for _, b := range Builtins() {
		if b.Version > 0 {
			stamped[b.Name] = b.Version
		}
	}

	// (1) Every stamped builtin must match its manifest entry exactly.
	for name, version := range stamped {
		want, ok := m.Roles[name]
		switch {
		case !ok:
			t.Errorf("builtin %q is stamped v%d but the manifest has no entry for it (unknown version)", name, version)
		case version < want:
			t.Errorf("builtin %q is stamped v%d but the library manifest is at v%d — the body is BEHIND upstream and needs a port (see the sync runbook)", name, version, want)
		case version > want:
			t.Errorf("builtin %q is stamped v%d, ahead of the manifest's v%d — the manifest claims a version it does not have", name, version, want)
		}
	}

	// (2) Every manifest role must correspond to a stamped builtin (keeps the
	// manifest honest — no stale/extra entry). The manifest is the only roster
	// this test iterates, by design: it does NOT walk the 14 upstream library
	// roles, so the 3 uzi-omitted roles never enter the check. Version-equality
	// for the roles present is already covered by (1).
	for name := range m.Roles {
		if _, ok := stamped[name]; !ok {
			t.Errorf("manifest lists role %q but no stamped builtin has that name — stale/extra manifest entry", name)
		}
	}

	// (3) Positive control against a vacuous pass: both sides non-empty, and the
	// number of stamped builtins checked equals the number of manifest roles.
	// Asserting set-size equality (rather than hardcoding 11) keeps adding a
	// future role a one-place edit.
	if len(stamped) == 0 {
		t.Fatalf("no stamped builtins found — the drift check would pass vacuously")
	}
	if len(m.Roles) == 0 {
		t.Fatalf("manifest has no roles — the drift check would pass vacuously")
	}
	if len(stamped) != len(m.Roles) {
		t.Errorf("stamped builtins (%d) and manifest roles (%d) differ in count — the manifest must list exactly the shipped stamped builtins", len(stamped), len(m.Roles))
	}
}
