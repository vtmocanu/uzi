package apitypes

// BuildInfoDTO is the unauthenticated GET /api/version response (PRD #175): the set
// of coordinates a deployed instance can state about itself.
//
// The route is unauthenticated and unrate-limited, so this struct's fields are
// world-readable to anyone who can reach the ingress. Most are public by construction
// — the image tag is in the chart, the commit is in the repo. UptimeSeconds is the
// exception and is a deliberate disclosure rather than an already-public fact; the
// reasoning lives with the handler that enforces it, in internal/handler's Version.
//
// That is a standing constraint on this type, not a description of it: a new field is
// either already public, or a considered disclosure recorded where it is enforced.
// Hostnames, environment, filesystem paths and dependency inventories are exactly what
// a build-info endpoint conventionally leaks, and this response carries none of them.
//
// Version and Founded are always present. Everything else is OMITTED when unknown
// rather than zero-valued: a `dev` build reporting commit "" and built_at
// "0001-01-01T00:00:00Z" would claim to know things it does not, and omission keeps
// "we don't know" distinguishable from "the value is empty".
type BuildInfoDTO struct {
	// Version is the Model-B release coordinate (== image tag == chart appVersion),
	// bare — the Dockerfile strips a leading v and the SPA re-adds one for display.
	// "dev" on an un-stamped build.
	//
	// This key and this value are UNCHANGED from the single-string response this type
	// widened. web/src/components/AppShell.tsx renders it in the footer and
	// web/src/pages/WorkersSettings.tsx feeds it to PRD #113's worker upgrade
	// classification, so it gates a fleet feature and is not merely cosmetic.
	Version string `json:"version"`
	// Founded is the date of this project's first commit, YYYY-MM-DD. A const, not a
	// build stamp. The AGE is computed by the consumer from this: sending both would
	// create two sources of truth that disagree the moment a long-lived SPA session
	// crosses midnight, and sending founded alone means the age stays correct without
	// a release.
	Founded string `json:"founded"`
	// BuiltAt is when the image was built, RFC3339 in UTC. Omitted unless it was
	// stamped AND parsed — a mangled CI value degrades to unknown rather than to a
	// plausible-looking lie.
	BuiltAt string `json:"built_at,omitempty"`
	// Commit is the full 40-char source SHA the image was built from, and the server
	// enforces that: a stamp that is not 40 hex characters is omitted rather than
	// served (see isFullSHA in internal/handler). Full rather than short so the value
	// stays greppable and linkable. Omitted on an un-stamped build.
	//
	// Consumers render it differently and both are deliberate, so do not read this as
	// "consumers truncate": `uzi version` prints all 40, because a terminal is exactly
	// where greppable matters, while the SPA popover shortens it for a footer that has
	// no room. What the two must agree on is that the FULL value stays reachable
	// somewhere — that is the property PRD #175 asks for, not a display convention.
	Commit string `json:"commit,omitempty"`
	// Commits is the number of commits in the history the image was built from (PRD
	// #175 M3). It is the one field whose CI plumbing is separable from the rest, so
	// it is independently droppable: nothing may depend on its presence, and every
	// consumer must render correctly without it.
	Commits *int `json:"commits,omitempty"`
	// UptimeSeconds is how long this process has been serving. The only RUNTIME fact
	// in this struct, and the only field that is a decision rather than an already-
	// public value — see the Version handler for why it is published and what would
	// require re-deciding it.
	//
	// A POINTER for correctness, not tidiness: 0 is a legitimate uptime during a
	// process's first second, so `omitempty` on a bare int64 would conflate it with
	// unknown. Unknown is not hypothetical here — many tests build a Handler as a
	// struct literal rather than through New (see the clock() comment in
	// internal/handler/handler.go), leaving startedAt the zero time, where a naive
	// subtraction renders roughly two millennia.
	UptimeSeconds *int64 `json:"uptime_seconds,omitempty"`
}
