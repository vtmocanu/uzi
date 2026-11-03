package store

import (
	"strings"
	"testing"
)

// Issue #3859: the qualifying predicate must be ONE text everywhere it appears. It is the
// exclusion in SweepClaimedNeverStarted and the selection in the two charge statements; a
// drift between them either leaves a qualifying run to the uncharged reset (the loop returns)
// or charges a run the sweep would also reset. Whitespace is normalised because the
// statements indent differently.
const claimedNeverStartedPredicate = `EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.run_id = runs.id AND h.user_id = runs.user_id AND h.state = 'open' AND h.generation < runs.claim_generation)
AND runs.snapshot_departed_generation = runs.claim_generation
AND EXISTS (SELECT 1 FROM workers w WHERE w.id = runs.worker_id AND w.snapshot_register_nonce IS NOT NULL AND runs.snapshot_departed_nonce = w.snapshot_register_nonce AND runs.snapshot_departed_epoch <= w.snapshot_epoch)
AND NOT EXISTS (SELECT 1 FROM worker_active_runs a WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.claim_generation = runs.claim_generation)`

// claimedNeverStartedProtection is SweepClaimedNeverStarted's D11 protection clause, which the
// charge statements copy verbatim.
const claimedNeverStartedProtection = `AND (runs.kind = 'chat'
       OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                       WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                         AND a.terminal_pending_until > now()
                         AND a.claim_generation = runs.claim_generation)
           AND NOT EXISTS (SELECT 1 FROM workers w
                           WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))`

func normalizeSQL(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestClaimedNeverStartedPredicateIsOneText(t *testing.T) {
	predicate, protection := normalizeSQL(claimedNeverStartedPredicate), normalizeSQL(claimedNeverStartedProtection)
	for name, sql := range map[string]string{
		"SweepClaimedNeverStarted exclusion":           sweepClaimedNeverStarted,
		"LockClaimedNeverStartedContinuations":         lockClaimedNeverStartedContinuations,
		"chargeClaimedNeverStartedContinuationsLocked": chargeClaimedNeverStartedContinuationsLocked,
	} {
		if got := strings.Count(normalizeSQL(sql), predicate); got != 1 {
			t.Errorf("%s carries the qualifying predicate %d times, want exactly once", name, got)
		}
		if got := strings.Count(normalizeSQL(sql), protection); got < 1 {
			t.Errorf("%s lost the protection clause", name)
		}
	}
	if !strings.Contains(normalizeSQL(sweepClaimedNeverStarted), "AND NOT ("+predicate+")") {
		t.Error("SweepClaimedNeverStarted must exclude the predicate as AND NOT (<predicate>)")
	}
	// The sweep's three existing copies of the protection clause are untouched.
	if got := strings.Count(normalizeSQL(sweepClaimedNeverStarted), protection); got != 3 {
		t.Errorf("SweepClaimedNeverStarted carries the protection clause %d times, want 3", got)
	}
}
