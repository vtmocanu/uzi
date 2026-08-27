// Package workersize is the server-side registry of hosted-worker size presets
// (PRD #58 Decision 7). It is the size analogue of workertmpl: a small hardcoded
// list the API validates a user's provision request against, so an arbitrary
// string can never reach a pod spec as free text.
//
// NAMES ONLY, deliberately. The cpu/memory/volume quantities each preset resolves
// to live in the CONTROLLER's own copy of the constants, never here:
// hostedsvc/protocol.go pins that the api sends the preset NAME and "shipping
// resolved values would make the api the authority on a pod spec it is not allowed
// to know anything about" (Decision 1 — the api holds zero kube access). So this
// package answers exactly one question: is this a preset we ship?
//
// Keep Names in lockstep with the controller's preset table. The two copies are a
// deliberate consequence of the api and controller being separate Go modules (the
// module boundary is what keeps a kube client out of the api), and
// hostedsvc/testdata/hosted_sizes.json is the golden that pins this list across it —
// see hostedsvc/size_contract_test.go, which also records what that golden does and
// does not buy. Drift is not silent-but-harmless: the api would accept a size the
// controller cannot resolve, and the worker would provision, never be rendered, and
// sit pending until its token expires.
package workersize

// Names is the curated set, in display order (smallest first). It is pinned by
// hostedsvc/testdata/hosted_sizes.json, which is the registry contract — not by the
// wire golden next to it, which carries "s" and "l" only as sample values of one
// poll response ("m" appears nowhere in it) and was never a gate on this list.
//
// Lowercase, and that matters: these strings go on the wire verbatim as
// DesiredWorker.Size and are stored as workers.hosted_size. The UI upper-cases for
// display; nothing upper-cased is ever a value.
var Names = []string{"s", "m", "l"}

// Valid reports whether name is a known size preset (registry membership). Used at
// provision, where the choice must be one we actually ship.
//
// Membership, never a format check: unlike workertmpl — which pairs Valid with a
// WellFormed charset test because a worker SELF-REPORTS its template and an
// unknown-but-well-formed name is legitimate drift — a size is only ever chosen by
// a user for us to act on. There is no reporting path and therefore no drift to
// tolerate, so no WellFormed analogue exists here on purpose: a size we do not
// ship is an error, not a signal.
func Valid(name string) bool {
	for _, n := range Names {
		if n == name {
			return true
		}
	}
	return false
}
