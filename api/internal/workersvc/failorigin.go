package workersvc

// PRD #69 M7a Pass A (the SEAM): the server's authoritative copy of the fail_origin
// vocabulary, modelled EXACTLY on the rate_limit_type precedent above (see
// limitwait.go's rateLimitTypes / CoerceRateLimitType). The judge needs a TRUSTED,
// structured failure ORIGIN for each failed run; today that origin lives only in
// free-text failure_reason, which is never parsed. This is the closed enum stamped at
// each terminal-failure write site (migration 00126's CHECK).
//
// WHY THE ALLOWLIST IS THE WHOLE SANITIZER, and why it lives here rather than in the
// worker: identical to rate_limit_type's argument. The worker reports fail_origin as
// UNTRUSTED free text — whatever the report site attached — and CoerceFailOrigin maps
// everything outside this set to nil before it can reach the DB, a DTO, the feed or
// the judge. An allowlist closes length, control-char and injection concerns in one
// move; no worker-controlled byte survives it.
//
// WHERE THE UNKNOWN MAPPING DIFFERS FROM rate_limit_type, ON PURPOSE. CoerceRateLimitType
// maps an unrecognised value to the literal "unknown" — a real member of its stored
// vocabulary, because "rejected by a window we could not name" and "never parked" are
// different facts a support query must distinguish. fail_origin has no such member: an
// unrecognised worker value coerces to nil so it never fabricates a bogus CLASS the
// judge would then trust. The `failed` arm defaults that nil to 'agent_failure'
// separately (SetState), because a worker-reported failure with no explicit origin IS
// the judgeable agent-failure case — but that default is a decision made at the write
// site, not smuggled in by the coercer.

// failOrigins is the closed set migration 00126's CHECK enforces, in source order.
//
// Adding or removing a member here without moving 00126 in the same commit is caught
// by TestFailOriginVocabularyMatchesCheck, which parses the CHECK out of the migration
// and compares — so a drift reddens at `go test` rather than raising 23514 on a user's
// failed run.
var failOrigins = []string{
	"provisioning_failed",
	"credential_unavailable",
	"guardrail_blocked",
	"rate_limited",
	"run_timeout",
	"worker_lost",
	"agent_failure",
	"plan_rejected",
	"auto_stopped",
}

// failOriginSet is the lookup form. Built once; failOrigins stays the declaration so
// the vocabulary reads as a list in source order (same shape as rateLimitTypeSet).
var failOriginSet = func() map[string]bool {
	m := make(map[string]bool, len(failOrigins))
	for _, o := range failOrigins {
		m[o] = true
	}
	return m
}()

// AllFailOrigins returns the whole vocabulary, in a form a guard can enumerate.
//
// Returns a fresh slice for the same reason AllRateLimitTypes does: a package-level
// var would let one caller's append corrupt every other reader's view of a CLOSED set.
func AllFailOrigins() []string {
	out := make([]string, len(failOrigins))
	copy(out, failOrigins)
	return out
}

// CoerceFailOrigin maps a worker-reported fail_origin onto the stored vocabulary.
//
// Absent (nil) stays absent. Present-and-in-set passes through verbatim.
// Present-but-unrecognised becomes nil — NOT a placeholder member — so the coercer
// never invents a class; the caller decides what a classless failure means (the
// `failed` arm defaults it to 'agent_failure'). Never an error: a terminal report is
// never failed on a technicality, mirroring CoerceRateLimitType's stated principle.
func CoerceFailOrigin(reported *string) *string {
	if reported == nil {
		return nil
	}
	if failOriginSet[*reported] {
		return reported
	}
	return nil
}
