package store

import (
	"regexp"
	"strings"
	"testing"
)

// Issue #3859: the qualifying predicates must be ONE text everywhere they appear. They are the
// exclusions and the final recheck in SweepClaimedNeverStarted and the selection and write in
// the charge and unwitnessed-park statements; a drift between them either leaves a qualifying
// run to the uncharged reset (the loop returns) or charges/parks a run the sweep would also
// reset. Whitespace is normalised because the statements indent differently.
//
// The constants are written here independently of the SQL and compared against it.
const (
	// eligibility: a custody continuation (open hold of an EARLIER generation), not
	// policy-unresolved, not a cross_check, no pending server-stamped stop, claim not released.
	cnsEligibility = `runs.kind <> 'cross_check'
AND COALESCE(runs.stop_kind, '') = ''
AND runs.claim_released_at IS NULL
AND runs.hold_reason IS DISTINCT FROM 'codex_policy_hold'
AND (runs.policy_outcome_id IS NULL OR runs.policy_resolved_at IS NOT NULL)
AND EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.run_id = runs.id AND h.user_id = runs.user_id AND h.state = 'open' AND h.generation < runs.claim_generation)`

	cnsDeparted = `runs.snapshot_departed_generation = runs.claim_generation
AND EXISTS (SELECT 1 FROM workers w WHERE w.id = runs.worker_id AND w.snapshot_register_nonce IS NOT NULL AND runs.snapshot_departed_nonce = w.snapshot_register_nonce AND runs.snapshot_departed_epoch <= w.snapshot_epoch)
AND NOT EXISTS (SELECT 1 FROM worker_active_runs a WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.claim_generation = runs.claim_generation)`

	// presence: online worker, no departure stamp for this generation, and presence under the
	// current registration (p1 registration evidence, or p2 a non-terminal-pending listing).
	cnsPresence = `runs.snapshot_departed_generation IS DISTINCT FROM runs.claim_generation
AND EXISTS (SELECT 1 FROM workers w WHERE w.id = runs.worker_id AND w.status = 'online'
AND ((runs.registration_evidence_generation = runs.claim_generation AND runs.registration_evidence_nonce = w.snapshot_register_nonce AND runs.registration_evidence_epoch >= w.snapshot_epoch)
OR EXISTS (SELECT 1 FROM worker_active_runs a WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.claim_generation = runs.claim_generation AND NOT a.terminal_pending)))`

	// cnsProtection is SweepClaimedNeverStarted's D11 protection clause, which the charge and
	// park statements copy verbatim.
	cnsProtection = `(runs.kind = 'chat'
       OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                       WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                         AND a.terminal_pending_until > now()
                         AND a.claim_generation = runs.claim_generation)
           AND NOT EXISTS (SELECT 1 FROM workers w
                           WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))`
)

var sqlParam = regexp.MustCompile(`\$\d+`)

// normalizeSQL collapses whitespace and renames positional parameters ($3) to $N, so the
// generated statements compare against the named-parameter text written in this file.
func normalizeSQL(s string) string {
	return sqlParam.ReplaceAllString(strings.Join(strings.Fields(s), " "), "$$N")
}

func TestClaimedNeverStartedPredicatesAreOneText(t *testing.T) {
	elig, departed, presence, prot := normalizeSQL(cnsEligibility), normalizeSQL(cnsDeparted), normalizeSQL(cnsPresence), normalizeSQL(cnsProtection)
	charge := elig + " AND " + departed
	park := elig + " AND " + presence
	deferral := "AND NOT COALESCE((" + park + " AND runs.claimed_at >= $N::timestamptz), false)"
	exclusion := "AND NOT COALESCE((" + charge + "), false)"

	count := func(sql, needle string) int { return strings.Count(normalizeSQL(sql), needle) }
	for name, c := range map[string]struct {
		sql                   string
		charge, park, windows int
		protection            int
	}{
		"LockClaimedNeverStartedContinuations":         {lockClaimedNeverStartedContinuations, 1, 0, 0, 1},
		"chargeClaimedNeverStartedContinuationsLocked": {chargeClaimedNeverStartedContinuationsLocked, 1, 0, 0, 1},
		"LockUnwitnessedContinuations":                 {lockUnwitnessedContinuations, 0, 1, 1, 1},
		"parkUnwitnessedContinuationsLocked":           {parkUnwitnessedContinuationsLocked, 0, 1, 1, 1},
		// The sweep carries the exclusion in the candidate set and again in the final UPDATE.
		"SweepClaimedNeverStarted": {sweepClaimedNeverStarted, 2, 2, 0, 3},
	} {
		if got := count(c.sql, charge); got != c.charge {
			t.Errorf("%s carries the charge predicate %d times, want %d", name, got, c.charge)
		}
		if got := count(c.sql, park); got != c.park {
			t.Errorf("%s carries the presence predicate %d times, want %d", name, got, c.park)
		}
		if got := count(c.sql, prot); got != c.protection {
			t.Errorf("%s carries the protection clause %d times, want %d", name, got, c.protection)
		}
		if c.windows > 0 && count(c.sql, park+" AND runs.claimed_at < $N::timestamptz") != c.windows {
			t.Errorf("%s must carry the presence predicate with the window negated %d time(s)", name, c.windows)
		}
	}
	// The departed text only ever appears inside the charge predicate, so an edit to one copy
	// cannot silently diverge from the others.
	for name, sql := range map[string]string{
		"LockClaimedNeverStartedContinuations":         lockClaimedNeverStartedContinuations,
		"chargeClaimedNeverStartedContinuationsLocked": chargeClaimedNeverStartedContinuationsLocked,
		"SweepClaimedNeverStarted":                     sweepClaimedNeverStarted,
	} {
		if count(sql, departed) != count(sql, charge) {
			t.Errorf("%s carries a departed-witness text outside the charge predicate", name)
		}
	}
	// In the sweep, both exclusions are COALESCE-wrapped so a NULL witness never excludes.
	if got := count(sweepClaimedNeverStarted, exclusion); got != 2 {
		t.Errorf("SweepClaimedNeverStarted carries the COALESCE-safe charge exclusion %d times, want 2", got)
	}
	if got := count(sweepClaimedNeverStarted, deferral); got != 2 {
		t.Errorf("SweepClaimedNeverStarted carries the COALESCE-safe witness deferral %d times, want 2", got)
	}
	if strings.Contains(normalizeSQL(sweepClaimedNeverStarted), "AND NOT (EXISTS (SELECT 1 FROM recovery_custody_holds") {
		t.Error("SweepClaimedNeverStarted still carries the NULL-unsafe AND NOT (...) exclusion")
	}
}
