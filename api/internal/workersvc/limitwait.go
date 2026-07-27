package workersvc

// Anthropic usage-limit park (PRD #35): the server's half of the rate_limit_type
// vocabulary.
//
// The worker reports `rate_limit_type` as UNTRUSTED free text — it is whatever the
// SDK frame carried, forwarded verbatim on purpose so the authoritative copy of the
// vocabulary lives on this side of the wire rather than in the agent. Everything
// that reaches the database, a DTO, the run feed or Slack passes through
// CoerceRateLimitType first.
//
// WHY THE ALLOWLIST IS THE WHOLE SANITIZER. The state path has no
// sanitizeSelfReported (that is the register path's), and this field deliberately
// does not get stripNULParam either. It does not need it: an allowlist that maps
// everything outside a seven-member set to the literal "unknown" is strictly
// stronger than any stripping filter, because no worker-controlled byte survives it
// at all. Length caps, control characters, NUL, and injection are closed in one
// move rather than four.
//
// WHY THIS LIVES IN workersvc RATHER THAN IN ITS OWN PACKAGE, unlike
// autoselect.Reason, whose comment argues the opposite for a superficially similar
// vocabulary. Reason has THREE enumerating guards (workersvc's, cmd/uzi's, and the
// web's), two of which must not import the server, so a shared home was the only
// way to stop it drifting. rate_limit_type has exactly one producer — this package,
// on the state path — and no consumer that must enumerate it: the DTO, the CLI and
// the Slack line all render whatever string they are handed, and are required to
// render an unrecognised one honestly rather than dropping it. One producer, one
// home.

// RateLimitTypeUnknown is what every unrecognised report becomes. It is a real
// member of the stored vocabulary (migration 00090's CHECK admits it), not a NULL:
// "the run was rejected by a window we could not name" and "the run never parked"
// are different facts and a support query must be able to tell them apart.
const RateLimitTypeUnknown = "unknown"

// rateLimitTypes is the closed set migration 00090's CHECK enforces: the six
// members of SDKRateLimitInfo.rateLimitType at the pinned
// @anthropic-ai/claude-agent-sdk (sdk.d.ts), plus RateLimitTypeUnknown.
//
// An SDK pin bump adding a seventh member is the ONE thing that moves this list,
// and it must move 00090 in the same commit — TestRateLimitTypeVocabularyMatchesCheck
// parses the CHECK out of the migration file and compares, so forgetting either
// half reddens at `go test` instead of raising 23514 on a user's run.
var rateLimitTypes = []string{
	"five_hour",
	"seven_day",
	"seven_day_opus",
	"seven_day_sonnet",
	"seven_day_overage_included",
	"overage",
	RateLimitTypeUnknown,
}

// rateLimitTypeSet is the lookup form. Built once; rateLimitTypes stays the
// declaration so the vocabulary reads as a list in source order.
var rateLimitTypeSet = func() map[string]bool {
	m := make(map[string]bool, len(rateLimitTypes))
	for _, t := range rateLimitTypes {
		m[t] = true
	}
	return m
}()

// AllRateLimitTypes returns the whole vocabulary, in a form a guard can enumerate.
//
// Returns a fresh slice for the same reason autoselect.AllReasons does: a
// package-level var would let one caller's append corrupt every other reader's view
// of a CLOSED set.
func AllRateLimitTypes() []string {
	out := make([]string, len(rateLimitTypes))
	copy(out, rateLimitTypes)
	return out
}

// CoerceRateLimitType maps a worker-reported rate_limit_type onto the stored
// vocabulary.
//
// Absent (nil) stays absent — a report that carried no type must not invent
// "unknown", because a NULL column and an "unknown" column mean different things
// (see RateLimitTypeUnknown). Present-but-unrecognised becomes "unknown", never an
// error: per the design brief's §7.8 constraint, a terminal report is NEVER failed
// on a technicality, mirroring runningStateParams' stated principle rather than
// departing from it.
func CoerceRateLimitType(reported *string) *string {
	if reported == nil {
		return nil
	}
	if rateLimitTypeSet[*reported] {
		return reported
	}
	unknown := RateLimitTypeUnknown
	return &unknown
}
