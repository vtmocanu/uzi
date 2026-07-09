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

// DefaultName is the template a worker runs when none is chosen — today's minimal
// image, matching the WORKER_TEMPLATE compose default.
const DefaultName = "base"

// Names is the curated set, in display order (base first). Mirror of
// agent/templates/<name>/Dockerfile.
var Names = []string{"base", "jvm"}

// Valid reports whether name is a known declared template.
func Valid(name string) bool {
	for _, n := range Names {
		if n == name {
			return true
		}
	}
	return false
}
