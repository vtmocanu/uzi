// Package planpolicy holds a deterministic "bright-line" content screen for a
// create-time SEEDED plan body.
//
// It exists as a leaf package for the same reason secretscrub does: the screen is
// a small, dependency-free content check that the run-lifecycle core applies to a
// seeded plan before it is trusted, and keeping it here avoids dragging heavier
// callers into a one-way dependency. The input is expected to be already
// secret-scrubbed (see secretscrub); this screen is orthogonal — it looks for
// infrastructure-reconnaissance TARGETS rather than credential shapes.
//
// The denylist is intentionally minimal and bright-line: it names only
// infrastructure endpoints that have no legitimate reason to appear in a
// repository code-change plan (cloud instance metadata endpoints, the default
// kube-apiserver ClusterIP, and the in-pod service-account token mount). It
// deliberately does NOT match things that CAN legitimately appear in a plan for
// this repo — kubernetes.default.svc DNS (this repo ships k8s manifests) nor
// cloud credential file paths such as .aws/credentials. Metadata-IP blocking is
// documented elsewhere as prose prior art (see forge.isDisallowedLogIP), but this
// is a text denylist, not a network control.
//
// This is defense-in-depth, never a licence: trivial obfuscation defeats a text
// denylist, so it complements — does not replace — network egress enforcement.
package planpolicy

import "regexp"

// The bright-line reconnaissance targets. Each rule pairs a case-insensitive
// compiled regexp with a fixed human-readable category. Boundary anchors (\b) keep
// the numeric literals from matching a longer surrounding number.
var (
	// imdsPattern matches the IMDS link-local IPv4 literal used by all major
	// clouds (169.254.169.254) and the GCP metadata DNS name.
	imdsPattern = regexp.MustCompile(`(?i)\b169\.254\.169\.254\b|metadata\.google\.internal`)

	// apiserverPattern matches the default kube-apiserver ClusterIP literal.
	// Anchored so it does not match 10.96.0.10 or 110.96.0.1.
	apiserverPattern = regexp.MustCompile(`(?i)\b10\.96\.0\.1\b`)

	// saTokenPattern matches the in-pod service-account token mount path. The
	// `/var/` prefix is optional: /var/run is an FHS symlink to /run, so
	// /run/secrets/kubernetes.io/serviceaccount names the identical file and is
	// the more canonical spelling — both forms must match.
	saTokenPattern = regexp.MustCompile(`(?i)/(?:var/)?run/secrets/kubernetes\.io/serviceaccount`)
)

// rule is one bright-line entry: a compiled regexp and the category reported when
// it is the first to match.
type rule struct {
	re       *regexp.Regexp
	category string
}

// rules is evaluated in order; Screen returns on the first match. Keeping them in
// an ordered slice makes the evaluation order and any future additions obvious.
var rules = []rule{
	{imdsPattern, "cloud instance metadata endpoint"},
	{apiserverPattern, "kube-apiserver ClusterIP"},
	{saTokenPattern, "in-pod service-account token mount"},
}

// Screen scans a (already secret-scrubbed) seeded-plan body for bright-line
// infrastructure-reconnaissance targets that have no legitimate reason to
// appear in a repository code-change plan. It returns the human-readable
// category of the FIRST rule that matches and matched=true; ("", false) when
// the plan is clean.
func Screen(plan string) (target string, matched bool) {
	for _, r := range rules {
		if r.re.MatchString(plan) {
			return r.category, true
		}
	}
	return "", false
}
