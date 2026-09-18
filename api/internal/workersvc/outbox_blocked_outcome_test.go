package workersvc

// PRD #1391 M3 (D13) — RunBlockedOutcome, the data source for the GetRun
// outcome_pending overlay. A worker whose terminal report the api permanently
// refused reports a blocked_reason on its heartbeat outbox entry; the api surfaces
// it on the run so the owner can resolve it with a discarding cancel. Two things
// must hold and are pinned here: only a recognised member of the CLOSED reason set
// is ever passed through (the worker is untrusted, so arbitrary text is dropped),
// and the reporting worker id is returned so the caller can owner-gate.

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRunBlockedOutcomeReturnsRecognisedReasonAndReporter(t *testing.T) {
	// Every member of the closed set is passed through, together with the worker that
	// reported it (so the handler can owner-gate on the run's current owner).
	for _, reason := range []string{"completion_permit_mismatch", "gap_unrecoverable", "reserve_exhausted"} {
		t.Run(reason, func(t *testing.T) {
			svc := New(nil, nil, testParams())
			runID := uuid.New()
			worker := uuid.New()
			svc.outbox.record(worker, []OutboxEntry{
				{RunID: runID, PendingTerminal: 1, BlockedReason: reason, Since: t0.Add(-time.Minute)},
			}, t0)

			gotReason, gotReporter, ok := svc.RunBlockedOutcome(runID)
			if !ok {
				t.Fatalf("RunBlockedOutcome ok = false, want true for recognised reason %q", reason)
			}
			if gotReason != reason {
				t.Fatalf("reason = %q, want %q", gotReason, reason)
			}
			if gotReporter != worker {
				t.Fatalf("reporter = %s, want the reporting worker %s (needed for the owner-gate)", gotReporter, worker)
			}
		})
	}
}

func TestRunBlockedOutcomeDropsUnrecognisedReason(t *testing.T) {
	// UNTRUSTED-TEXT HARDENING: the worker's blocked_reason is free text on the wire,
	// so a value outside the closed set — arbitrary text, or an injected string — is
	// dropped (ok == false), never surfaced onto the owner's run page. Empty text (the
	// Run A shape, where nothing is blocked) drops the same way.
	for _, reason := range []string{"", "stalled", "arbitrary worker text", "COMPLETION_PERMIT_MISMATCH"} {
		t.Run("reason="+reason, func(t *testing.T) {
			svc := New(nil, nil, testParams())
			runID := uuid.New()
			svc.outbox.record(uuid.New(), []OutboxEntry{
				{RunID: runID, PendingTerminal: 1, BlockedReason: reason, Since: t0.Add(-time.Minute)},
			}, t0)

			if _, _, ok := svc.RunBlockedOutcome(runID); ok {
				t.Fatalf("RunBlockedOutcome ok = true for unrecognised reason %q, want it dropped", reason)
			}
		})
	}
}

func TestRunBlockedOutcomeUntrackedRun(t *testing.T) {
	// A run with no tracked outbox entry (the common case — no worker reported a blocked
	// terminal) yields ok == false, so the overlay leaves outcome_pending null.
	svc := New(nil, nil, testParams())
	if _, _, ok := svc.RunBlockedOutcome(uuid.New()); ok {
		t.Fatal("RunBlockedOutcome ok = true for an untracked run, want false")
	}
}
