package autoselect

import (
	"time"

	"github.com/google/uuid"
)

// BindingWindowReset is resetKey exported: the reset of the window that ACTUALLY
// binds this candidate, i.e. whichever of the five-hour and seven-day windows
// produced headroom's min (PRD #111 D22).
//
// A thin wrapper rather than a second implementation, deliberately. The
// binding-window rule is subtle in exactly the way that invites a plausible-looking
// copy: min(five, seven) is the obvious reading and it is WRONG, because the
// tie-break asks which token is about to replenish and only the binding window's
// replenishment raises headroom. A duplicate would drift silently and no test would
// notice, which is the drift D21 exists to prevent.
//
// The bool is false for "+∞": the candidate is unmeasured, or the binding window
// names no reset at all (00080 writes NULL when Anthropic reports none). A zero
// time.Time is NOT the sentinel — it is a real, orderable instant that would sort
// before every genuine reset and invert the rule.
func BindingWindowReset(c Candidate) (time.Time, bool) { return resetKey(c) }

// NextAvailable answers ONE question, for PRD #35 Decision 6e: at the earliest, when
// could this user spend something OTHER than the credential that just died?
//
// It exists because `retry_not_before` gates PROMOTION (limit_wait → queued, in the
// sweeper) while credential re-selection happens at CLAIM, which is strictly
// downstream of promotion. There is no instant at which the newly chosen credential
// is known and the gate has not already fired — so the pool-awareness has to be
// stamped at PARK time, which is here, or not exist at all. The alternative the PRD
// first proposed (recompute the stamp from the re-selected credential) is not
// buildable for that reason; adr/0035 records the full argument.
//
// # What each candidate contributes
//
//	StatusEligible                                    now — spendable right now
//	StatusBelowThreshold (Measured, under MinHeadroom) its BINDING-window reset
//	NotPooled / NoReading / Unmeasured / Stale        nothing
//
// The floor is the minimum over the contributors. Nothing contributing means
// ok=false and the caller keeps whatever base it had.
//
// # Why an unknown contributes NOTHING rather than `now` or `+∞`
//
// This is the load-bearing asymmetry and both alternatives are actively wrong. If an
// unknown pulled the floor to `now`, a poller-disabled deployment (every candidate
// classifies Stale, which is what UZI_USAGE_POLL_INTERVAL=0 produces for every
// token — see Policy.MaxStaleness) would promote every parked run instantly and
// thrash it straight back into the same exhausted window. If an unknown pushed the
// floor out, one never-polled token would delay every park for a user who has a
// perfectly good second credential. Contributing nothing is the only reading under
// which a deployment with no gauge data behaves EXACTLY as it does today: no
// candidate contributes, ok is false, and the caller falls back to the dead
// credential's own reset.
//
// # Why StatusBelowThreshold contributes its reset rather than being skipped
//
// Below-threshold means "measurably low", not "unusable" — D10's best-of-pool
// fallback picks exactly these when nothing better exists. Its binding window is the
// one whose replenishment would raise its headroom, so that reset is a real,
// defensible lower bound on when this user regains capacity. Skipping it would lose
// the one signal a multi-token user has.
//
// # The result is a FLOOR, not a promise
//
// It is a lower bound on "something is spendable", computed from a gauge with
// poll-interval lag. Events after the park can invalidate it — another run consumes
// the alternative — and the cost of being wrong is one re-park against the
// RUN_LIMIT_MAX_WAITS budget. Stated residual, accepted: a user with more pooled
// credentials than that budget can burn all of it cycling through them, after which
// the run fails with "usage-limit retry budget exhausted", which is honest.
//
// # exclude
//
// The credential that just died, which must not vote for its own replacement. A
// uuid.Nil exclusion returns ok=false rather than "exclude nothing", and that refusal
// lives HERE rather than in the caller on purpose: the caller reaches this with a
// nil runs.anthropic_secret_id whenever a run predates PRD #111 or its credential
// recording failed, and without the exclusion id leg 1 would fire on the dead
// credential's OWN stale-but-eligible reading and promote the run instantly into the
// window it just exhausted. A precondition closed inside the pure function is
// testable; the same precondition written as a caller's `if` is one refactor from
// being dropped.
//
// Total: any input, including a nil slice, yields an answer.
func NextAvailable(cands []Candidate, exclude uuid.UUID, p Policy, now time.Time) (time.Time, bool) {
	if exclude == uuid.Nil {
		return time.Time{}, false
	}
	var floor time.Time
	found := false
	consider := func(t time.Time) {
		if !found || t.Before(floor) {
			floor, found = t, true
		}
	}
	for _, c := range cands {
		if c.SecretID == exclude {
			continue
		}
		switch Classify(c, p, now).Status {
		case StatusEligible:
			consider(now)
		case StatusBelowThreshold:
			// BindingWindowReset, never min(five, seven): only the binding window's
			// replenishment moves this candidate's headroom. A candidate whose binding
			// window names no reset contributes nothing, same as an unknown.
			if r, ok := BindingWindowReset(c); ok {
				consider(r)
			}
		}
	}
	return floor, found
}
