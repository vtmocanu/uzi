package workersvc

import (
	"regexp"

	"github.com/vtmocanu/uzi/api/internal/toolprofile"
)

// deniedCLITargetSep splits a recommendation target into candidate tokens. Targets are
// free text — "file, glab" is a real observed example (a rec whose target names a file
// AND the barred glab CLI) — so a single equality check against the whole string would
// miss the mixed case, which is exactly what the net must catch.
var deniedCLITargetSep = regexp.MustCompile(`[,/\s]+`)

// recommendsDeniedExecutable reports whether a recommendation target names a denylisted,
// credential-bearing CLI (glab/gh/aws/az/…). It tokenises the free-text target on
// [,/\s]+ and returns true if ANY non-empty token is a toolprofile.DeniedExecutable — a
// PARTIAL match dismisses, because the mixed "file, glab" case is precisely what must be
// caught (issue #167). toolprofile.DeniedExecutable already basenames and trims each
// token, so a path-form token ("/usr/local/bin/gh") resolves to its executable name.
func recommendsDeniedExecutable(target string) bool {
	for _, tok := range deniedCLITargetSep.Split(target, -1) {
		if tok == "" {
			continue
		}
		if toolprofile.DeniedExecutable(tok) {
			return true
		}
	}
	return false
}
