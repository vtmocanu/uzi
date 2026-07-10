// Package workertmpl is the server-side registry of curated worker templates
// (PRD #18). The templates themselves live in git under agent/templates/<name>/;
// this list is the small hardcoded mirror the API validates a user's *declared*
// choice against at join-token issuance. It is deliberately a code constant, not
// a DB table or a runtime scan of the agent image: the templates are reviewed
// like any code, the API runs in a different container from the worker image, and
// a fixed set keeps issuance validation trivial.
//
// Keep Names in lockstep with agent/templates/*/ (one entry per directory). A
// worker's *reported* template is NOT validated here — an unknown reported name
// is precisely the drift signal the UI badges (Decision 5).
package workertmpl

import "regexp"

// DefaultName is the template a worker runs when none is chosen — today's minimal
// image, matching the WORKER_TEMPLATE compose default.
const DefaultName = "base"

// Names is the curated set, in display order (base first). Mirror of
// agent/templates/<name>/Dockerfile.
var Names = []string{"base", "jvm"}

// nameRe bounds a well-formed template name: lowercase kebab, 1–64 chars. This is
// the charset a directory name under agent/templates/ can legally have, and the
// bound we require before letting a worker's SELF-REPORTED template near the DB or
// the web UI (it is untrusted input). It is a format check, not membership: an
// unknown-but-well-formed reported name is the drift signal, so it is allowed.
var nameRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// Valid reports whether name is a known DECLARED template (registry membership).
// Used at join-token issuance, where the choice must be one we actually ship.
func Valid(name string) bool {
	for _, n := range Names {
		if n == name {
			return true
		}
	}
	return false
}

// WellFormed reports whether name is safe to persist/surface as a worker's
// self-reported template: tight charset + length, no path separators, no dots, no
// whitespace. The server drops anything failing this rather than storing raw
// worker input (a hostile worker can send arbitrary bytes). Membership is NOT
// required — an unknown well-formed name is legitimate drift.
func WellFormed(name string) bool {
	return nameRe.MatchString(name)
}
