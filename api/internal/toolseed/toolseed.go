// Package toolseed knows which packages the baked worker image toolchain can
// actually supply, so the api can gate the tier-1 tool allowlist to that set
// (PRD #123 M3). The allowlist governs PERMISSION; the seed governs
// AVAILABILITY. A package that is allowlisted but not baked is permitted yet
// unprovisionable behind the worker egress block, so a run requesting it hangs
// then fails at 0 iterations. This package turns that run-time failure into an
// admin-time error by exposing Covered.
//
// The seed the worker installs lives in agent/devbox-global/devbox.json, outside
// this Go module's package directory. go:embed cannot reach it (the pattern may
// not leave the embedding package's directory — verified empirically), so per
// Decision 5 we ship a BYTE-IDENTICAL generated copy here (devbox.json) plus a
// golden test (TestSeedCopyMatchesManifest) that fails the moment the copy drifts
// from the manifest. Re-copy with:
//
//	cp agent/devbox-global/devbox.json api/internal/toolseed/devbox.json
//
// This is a dependency-free leaf package on purpose: it does not import
// toolprofile (the @version split is inlined below) so anything may depend on it.
package toolseed

import (
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed devbox.json
var seedManifest []byte

// nixOutputSuffixes are the nixpkgs multi-output names an attr may carry as a
// trailing `.<output>` selector (e.g. openssl.bin, file.out, jq.bin,
// shellcheck.bin). An admin allowlists the plain package name (openssl, file,
// jq), so we strip the output selector when building the covered set. This is a
// closed set of OUTPUT names, deliberately: `python3Packages.pip` ends in `.pip`,
// but `pip` is not an output name, so it is left intact and its base
// `python3Packages` never appears (pip is covered as `python3Packages.pip` if an
// admin ever allowlisted that exact attr, which they do not — see below).
var nixOutputSuffixes = map[string]bool{
	"bin":   true,
	"out":   true,
	"dev":   true,
	"man":   true,
	"doc":   true,
	"lib":   true,
	"info":  true,
	"debug": true,
	"dist":  true,
}

// seedAliases maps a seed attr's normalized name to the allowlist name that names
// the same tool. This is the ONE genuine package alias: the seed bakes the Go
// `yq-go` package, whose binary on PATH is `yq` — which is the allowlist name. So
// coverage for yq is asserted at the binary-on-PATH level, a documented caveat.
//
// Deliberately NOT aliased to their binary names: go-task (binary `task`), gnumake
// (binary `make`), kubernetes-helm (binary `helm`), python3Packages.pip (binary
// `pip`). An admin allowlists the devbox PACKAGE name, not the binary name, so
// allowlisting `go-task` is covered but `task` is not — and `devbox install task`
// would resolve a DIFFERENT nixpkgs attr, so treating `task` as covered would be
// wrong. yq-go→yq is the sole exception because the swap is a naming quirk of that
// one package, not a package-vs-binary distinction an admin could get wrong.
var seedAliases = map[string]string{
	"yq-go": "yq",
}

// seedExceptions are packages that are allowlisted but deliberately NOT baked into
// the worker image, grandfathered before this gate existed (PRD #123 M3,
// Decision 4). They are documented operability limits, not coverage:
//   - kubectl: a worker can reach no cluster by construction
//     (automountServiceAccountToken:false at both the ServiceAccount and the pod
//     spec, and no kubeconfig is injected — see agent/devbox-global/devbox.json),
//     so baking it buys nothing and it stays off the toolchain but on the
//     allowlist.
//   - nodejs: nodejs is in the 00046 seed but is NOT baked as a devbox package;
//     the base image's node (node:22-alpine) is not a devbox-provisioned nodejs,
//     so the gate would otherwise reject the seeded row on every deployment.
//
// These are the only two entries the gate treats as covered without a
// corresponding baked package.
var seedExceptions = map[string]bool{
	"kubectl": true,
	"nodejs":  true,
}

// seedSet is the set of allowlist names the baked toolchain can supply, built once
// at init from the embedded manifest by normalizing each seed attr.
var seedSet = buildSeedSet()

// buildSeedSet parses the `packages` list from the embedded manifest (HuJSON) and
// normalizes each attr into the set of allowlist names the baked toolchain covers.
func buildSeedSet() map[string]bool {
	var m struct {
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(stripJSONComments(seedManifest), &m); err != nil {
		// A malformed embedded copy is a build/copy error the golden test catches;
		// fail closed here (empty set) rather than panic in a leaf init.
		return map[string]bool{}
	}
	set := make(map[string]bool, len(m.Packages))
	for _, attr := range m.Packages {
		set[Normalize(attr)] = true
	}
	return set
}

// Normalize maps a seed attr to the allowlist name it covers: it strips a trailing
// nixpkgs `.<output>` selector (jq.bin→jq, openssl.bin→openssl, file.out→file,
// shellcheck.bin→shellcheck) but leaves python3Packages.pip intact (pip is not an
// output name), then applies the alias map (yq-go→yq). Everything else is
// identity. Case-sensitive: nix attrs are case-sensitive.
func Normalize(attr string) string {
	if i := strings.LastIndexByte(attr, '.'); i >= 0 {
		if nixOutputSuffixes[attr[i+1:]] {
			attr = attr[:i]
		}
	}
	if alias, ok := seedAliases[attr]; ok {
		return alias
	}
	return attr
}

// Covered reports whether the baked worker toolchain can supply pkg — i.e. pkg
// (its base name, before any @version) is either in the seed manifest or a
// documented exception. Case-sensitive. This is the gate: an allowlist name that
// is not Covered must be added to the image (agent/devbox-global/devbox.json) and
// the image rolled before a run can provision it.
func Covered(pkg string) bool {
	base := pkg
	if i := strings.IndexByte(pkg, '@'); i >= 0 {
		base = pkg[:i]
	}
	return seedSet[base] || seedExceptions[base]
}

// stripJSONComments removes HuJSON `//` line comments and `/* */` block comments
// from the manifest so encoding/json can parse it. It is string-context aware: a
// comment marker inside a double-quoted JSON string is copied verbatim, and
// backslash escapes are honored so an escaped quote never ends the string early.
// Ported from agent/src/repo-tools.ts stripJsonComments (comment pass only —
// devbox.json carries no trailing commas, and encoding/json rejects them anyway,
// so no trailing-comma pass is needed here).
func stripJSONComments(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	inStr := false
	n := len(raw)
	for i := 0; i < n; {
		ch := raw[i]
		if inStr {
			out = append(out, ch)
			if ch == '\\' && i+1 < n {
				out = append(out, raw[i+1])
				i += 2
				continue
			}
			if ch == '"' {
				inStr = false
			}
			i++
			continue
		}
		if ch == '"' {
			inStr = true
			out = append(out, ch)
			i++
			continue
		}
		if ch == '/' && i+1 < n && raw[i+1] == '/' {
			i += 2
			for i < n && raw[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '/' && i+1 < n && raw[i+1] == '*' {
			i += 2
			for i < n && (raw[i] != '*' || i+1 >= n || raw[i+1] != '/') {
				i++
			}
			i += 2 // consume the closing */
			continue
		}
		out = append(out, ch)
		i++
	}
	return out
}
