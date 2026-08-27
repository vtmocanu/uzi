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
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/store"
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
	// `op` and `bw` above are NOT nixpkgs attribute names — verified at the pinned rev
	// (agent/devbox-global/devbox.lock), where both fail to evaluate. They were dead
	// entries: the evident intent was to bar the 1Password and Bitwarden CLIs, and the
	// real attributes are these, which were allowlistable the whole time. Added
	// 2026-07-27 to realize that intent, not to widen policy. The bare names stay as
	// defensive aliases, the same shape as `gcloud` beside `google-cloud-sdk`.
	"_1password-cli": true, "bitwarden-cli": true,
}

// Denied reports whether pkg's base name is a credential-bearing CLI barred by
// Decision 6, regardless of the allowlist.
func Denied(pkg string) bool {
	base, _ := Split(pkg)
	return denylist[base]
}

// DenylistNames returns the denylist's package BASE names, sorted. Shipped in the
// claim (ClaimConfig.DeniedToolPackages) so the worker can apply the SAME Decision 6
// policy to TIER-2 (repo devbox.json opt-in) packages, which the server filters by
// shape only and so never denylist-checks (PRD #123 M1b). These are package names,
// not executable names — see deniedPackageExecutables for the exec-name mapping the
// judge scan uses. It is a compile-time-constant list; callers may send it verbatim.
func DenylistNames() []string {
	names := make([]string, 0, len(denylist))
	for name := range denylist {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// deniedPackageExecutables maps each denylisted PACKAGE to the executables it puts on
// PATH. The denylist above is keyed by package name, which is right for provisioning
// (a package name is what an admin allowlists and what devbox installs) and wrong for
// anything observing a SHELL, where only the executable ever appears.
//
// The two diverge exactly where it would hurt: `awscli` installs `aws`, `azure-cli`
// installs `az`, `google-cloud-sdk` installs gcloud/gsutil/bq. A name-equality check
// against the denylist would cover glab and quietly miss every one of those.
//
// The key set MUST equal the denylist's key set; TestDeniedExecutablesCoverDenylist
// asserts it in both directions, so adding a package to the denylist without naming
// its executables fails the TEST rather than silently narrowing the check. (It fails
// the test, not the build — Go compiles fine either way. The distinction matters: a
// check that reports green while covering a subset is the exact defect the toolchain
// guard in agent/devbox-global was rewritten to stop.)
//
// An executable listed here must NOT also be a package that is absent from the
// denylist — otherwise an installable tool's CLI becomes permanently unreportable.
// That is not hypothetical: `flyctl` symlinks its binary to `fly`, but nixpkgs `fly`
// is the Concourse CI client, a different and allowlistable tool. Listing the alias
// suppressed Concourse's CLI. TestDeniedExecutablesAreNotInstallablePackages pins it.
var deniedPackageExecutables = map[string][]string{
	"glab": {"glab"},
	"gh":   {"gh"},
	"hub":  {"hub"},
	"tea":  {"tea"},
	// The AWS CLI ships as `aws` from either package generation, plus a completer.
	"awscli":      {"aws", "aws_completer"},
	"awscli2":     {"aws", "aws_completer"},
	"aws-sam-cli": {"sam"},
	"azure-cli":   {"az"},
	// google-cloud-sdk symlinks FIVE programs into bin, and the two easiest to forget
	// are the credential helpers — precisely the ones worth keeping unobservable.
	"google-cloud-sdk": {"gcloud", "gsutil", "bq", "git-credential-gcloud.sh", "docker-credential-gcloud"},
	"gcloud":           {"gcloud"},
	"kubelogin":        {"kubelogin"},
	"oci-cli":          {"oci"},
	"doctl":            {"doctl"},
	// NOT `fly`: that alias belongs to flyctl, but the nixpkgs package named `fly` is
	// the Concourse CI CLI, which is allowlistable. See the note above.
	"flyctl":         {"flyctl"},
	"heroku":         {"heroku"},
	"vault":          {"vault"},
	"op":             {"op"},
	"bw":             {"bw"},
	"_1password-cli": {"op"},
	"bitwarden-cli":  {"bw"},
}

// deniedExecutableSet is the flattened reverse index of deniedPackageExecutables,
// built once so lookups are O(1) on a hot scan path.
var deniedExecutableSet = func() map[string]bool {
	s := make(map[string]bool)
	for _, execs := range deniedPackageExecutables {
		for _, e := range execs {
			s[e] = true
		}
	}
	return s
}()

// DeniedExecutable reports whether cmd is an executable installed by a denylisted,
// credential-bearing package — i.e. a command that can never be added through the
// ALLOWLIST path, so observing it missing there is the policy working rather than a
// gap to report.
//
// SCOPE, stated precisely because the obvious stronger claim is false. The denylist is
// enforced on the TIER-1 path at three points — the admin allowlist write (via Denied),
// and profile save + claim assembly (via Resolve → Allowed, which reads the denylist map
// directly rather than going through Denied). The tier-2 path (a cloned repo's own
// devbox.json under repo_devbox_opt_in) is filtered by SHAPE server-side, and since
// PRD #123 M1b the denylist BASE NAMES also ship in the claim (DenylistNames →
// ClaimConfig.DeniedToolPackages) so the worker drops any denied tier-2 package by base
// name before provisioning (agent/src/repo-tools.ts filterDeniedPackages,
// provision-run.ts). This DeniedExecutable scan (exec names, judge path) is still not
// the tier-2 enforcement point — that is the worker package-name filter just named.
//
// (An earlier draft of this note said Denied() has "exactly one call site" and inferred
// tier-1 was bounded only at the allowlist write. The call-site count was right and the
// inference wrong — Allowed() reads the map without going through Denied, so a
// grandfathered allowlist row still cannot pass a denied package at claim time.)
//
// The residual: if a tier-2 install of such a tool fails, the run really is missing
// something it was meant to have and this suppression hides it. Not theoretical —
// vtmocanu/uzi itself has repo_devbox_opt_in true. Narrow, accepted, recorded.
//
// SECOND RESIDUAL, from the basenaming below: a repo-local script sharing a name with a
// denied CLI (`./scripts/vault`, `tools/op`) is suppressed too. Path forms were the
// bypass basenaming exists to close, so this is deliberate — a denied CLI leaking past
// the filter is worse than a repo script going unreported — but it is a real widening
// and not a corner case.
//
// Distinct from Denied, which takes a PACKAGE name. Callers that observe shell output
// (the judge's command-not-found scan) want this one.
//
// Basenames the path forms: the exec.LookPath error the scan matches carries a full
// path (`exec: "/usr/local/bin/glab": executable file not found`), so a bare map
// lookup would miss it — measured, before this was added. Mirrors Denied(), which
// normalizes with Split() for the same reason.
func DeniedExecutable(cmd string) bool {
	return deniedExecutableSet[path.Base(strings.TrimSpace(cmd))]
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
