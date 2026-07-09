// Package toolprofile resolves a run's tier-1 tool packages (PRD #18 M3) against
// an allowlist before they are delivered in the claim payload and provisioned by
// the worker's devbox engine.
//
// M3 ships a HARDCODED allowlist here and no desire source yet: assembleClaim
// resolves an empty desired set, so real runs provision nothing (no substituter
// egress in the default stack) while the whole worker path is exercised by tests.
// M4 replaces the hardcoded allowlist + empty desired set with the DB-backed
// tool_allowlist + per-(user,repo) repo_tool_profiles, reusing Resolve unchanged —
// keep this the single resolution seam.
//
// The allowlist is an operability control, not a sandbox (nix packages run build
// hooks); the actual security control is the worker's secret-scrubbed provisioning
// env (Decision 3). The allowlist bounds WHAT is installed, and deliberately
// excludes any pre-authenticated credential-bearing CLI (a logged-in glab, a
// kubeconfig-baked tool) that would bypass the "worker holds the PAT, agent
// doesn't" boundary (Decision 6).
package toolprofile

import (
	"regexp"
	"sort"
	"strings"
)

// Allowlist is the M3 hardcoded set of permitted package names (nix/devbox
// package identifiers). A version suffix (`name@1.2`) is allowed on any listed
// name — the base name must be here, the version is the caller's to pin. Kept
// intentionally small and CLI-tool-only; grows by code review until M4's DB
// allowlist supersedes it.
var Allowlist = map[string]bool{
	"kubectl":   true,
	"terraform": true,
	"jq":        true,
	"yq":        true,
	"ripgrep":   true,
	"fd":        true,
	"go":        true,
	"nodejs":    true,
	"python3":   true,
}

// pkgNameRe bounds a well-formed package token: a nix-ish package name with an
// optional `@version` suffix. Rejects shell metacharacters, paths, and spaces so a
// desired entry can't smuggle anything past the allowlist check.
var pkgNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]*(@[a-zA-Z0-9][a-zA-Z0-9._+-]*)?$`)

// baseName strips a trailing @version so the allowlist check is on the package
// identity, not the pin.
func baseName(pkg string) string {
	if i := strings.IndexByte(pkg, '@'); i >= 0 {
		return pkg[:i]
	}
	return pkg
}

// Allowed reports whether pkg is well-formed AND its base name is on the
// allowlist.
func Allowed(pkg string) bool {
	if !pkgNameRe.MatchString(pkg) {
		return false
	}
	return Allowlist[baseName(pkg)]
}

// Resolve filters desired down to the allowed, well-formed packages, returning the
// kept list (sorted, deduped) and the rejected list (for a clear error / audit).
// A nil/empty desired yields nil, nil — the M3 default (no provisioning).
func Resolve(desired []string) (allowed, rejected []string) {
	seen := map[string]bool{}
	for _, p := range desired {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !Allowed(p) {
			rejected = append(rejected, p)
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		allowed = append(allowed, p)
	}
	sort.Strings(allowed)
	return allowed, rejected
}
