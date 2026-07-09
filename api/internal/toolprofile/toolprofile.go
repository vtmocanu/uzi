// Package toolprofile validates and resolves a run's tier-1 tool packages
// (PRD #18) against an admin-managed allowlist before they are delivered in the
// claim payload and provisioned by the worker's devbox engine.
//
// M3 shipped a hardcoded allowlist here; M4 moves the allowlist to the DB
// (tool_allowlist) and the desired packages to the DB (repo_tool_profiles). This
// package stays pure — it operates on a Rules map the caller loads from the DB, so
// the same Resolve runs at profile write time and at claim time (the allowlist can
// shrink between them). Validation at BOTH points is the design (Technical §3): a
// save with an out-of-list package fails the save; a grandfathered package that
// falls out of the list later fails the claim (Success Criteria bullet 5).
//
// The allowlist is an operability control, not a sandbox (nix packages run build
// hooks); the actual security control is the worker's secret-scrubbed provisioning
// env (Decision 3). The allowlist bounds WHAT is installed, and must never include
// a pre-authenticated credential-bearing CLI (a logged-in glab, a kubeconfig-baked
// tool) that would bypass the "worker holds the PAT, agent doesn't" boundary
// (Decision 6).
package toolprofile

import (
	"regexp"
	"sort"
	"strings"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// maxPkgLen bounds a package token (name or name@version). The regex charset is
// otherwise unbounded; a length cap keeps a profile entry from being arbitrarily
// large.
const maxPkgLen = 128

// AllowRule is one allowlist entry's policy. PinnedVersion, when non-empty,
// requires a package to be requested at exactly that version (name@version); empty
// means any version (or none).
type AllowRule struct {
	PinnedVersion string
}

// Rules maps a permitted package base name to its policy — the DB allowlist
// projected into a lookup the pure functions here consume.
type Rules map[string]AllowRule

// RulesFromRows projects the DB allowlist rows into a Rules map. The SINGLE shared
// loader used at BOTH write time (the profile-save handler) and claim time (the
// worker's claim assembly), so the two can never drift — their agreement is
// load-bearing (a package allowed at save must resolve the same way at claim).
func RulesFromRows(rows []store.ToolAllowlist) Rules {
	rules := make(Rules, len(rows))
	for _, row := range rows {
		var pinned string
		if row.PinnedVersion.Valid {
			pinned = row.PinnedVersion.String
		}
		rules[row.Name] = AllowRule{PinnedVersion: pinned}
	}
	return rules
}

// denylist is the set of package BASE names that ship a pre-authenticated,
// credential-bearing CLI (Decision 6). They are rejected even if an admin
// allowlists one — a logged-in glab/gh/aws/az/gcloud reachable by the agent's Bash
// tool would bypass the "worker holds the PAT, agent doesn't" boundary. This turns
// Decision 6 from advisory prose into enforced policy. Keep it short + reviewed.
var denylist = map[string]bool{
	"glab": true, "gh": true, "hub": true, "tea": true,
	"awscli": true, "awscli2": true, "aws-sam-cli": true,
	"azure-cli": true, "google-cloud-sdk": true, "gcloud": true,
	"kubelogin": true, "oci-cli": true, "doctl": true, "flyctl": true,
	"heroku": true, "vault": true, "op": true, "bw": true,
}

// Denied reports whether pkg's base name is a credential-bearing CLI barred by
// Decision 6, regardless of the allowlist.
func Denied(pkg string) bool {
	base, _ := Split(pkg)
	return denylist[base]
}

// pkgNameRe bounds a well-formed package token: a nix-ish package name with an
// optional `@version` suffix. Rejects shell metacharacters, paths, and spaces so a
// desired entry can't smuggle anything past the allowlist check.
var pkgNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]*(@[a-zA-Z0-9][a-zA-Z0-9._+-]*)?$`)

// WellFormed reports whether pkg is a syntactically valid, bounded package token
// (name or name@version). Used at profile write time to reject junk entries before
// the allowlist check.
func WellFormed(pkg string) bool {
	return len(pkg) <= maxPkgLen && pkgNameRe.MatchString(pkg)
}

// versionRe bounds a version token (the pinned_version of an allowlist entry).
var versionRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]*$`)

// WellFormedVersion reports whether v is a safe, bounded version token — no
// metacharacters, paths, or spaces that could escape the allowlist match.
func WellFormedVersion(v string) bool {
	return len(v) <= maxPkgLen && versionRe.MatchString(v)
}

// Split separates a package token into its base name and version (empty when there
// is no `@version` suffix).
func Split(pkg string) (base, version string) {
	if i := strings.IndexByte(pkg, '@'); i >= 0 {
		return pkg[:i], pkg[i+1:]
	}
	return pkg, ""
}

// Allowed reports whether pkg is well-formed, NOT denied (Decision 6), AND
// permitted by rules: its base name must be on the allowlist, and if that rule pins
// a version, pkg must request exactly it.
func Allowed(pkg string, rules Rules) bool {
	if !WellFormed(pkg) {
		return false
	}
	base, version := Split(pkg)
	if denylist[base] {
		return false // credential-bearing CLI, barred regardless of the allowlist
	}
	rule, ok := rules[base]
	if !ok {
		return false
	}
	if rule.PinnedVersion != "" {
		return version == rule.PinnedVersion
	}
	return true
}

// Resolve filters desired down to the allowed, well-formed packages against rules,
// returning the kept list (sorted, deduped) and the rejected list (in input order,
// for a clear error / audit). A nil/empty desired yields nil, nil.
func Resolve(desired []string, rules Rules) (allowed, rejected []string) {
	seen := map[string]bool{}
	for _, p := range desired {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !Allowed(p, rules) {
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
