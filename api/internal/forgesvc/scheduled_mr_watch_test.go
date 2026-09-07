package forgesvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The board-FREE scheduled MR-state recorder (SyncScheduledMRStates, PRD #908) is a
// second implementation of syncOneMRState's observe->persist->cancel contract WITHOUT
// the board move. These tests pin the differential/mirror contract: every fixture in
// TestSyncScheduledMRStatesDifferential asserts recorded state, cancel-call count, AND
// zero board moves (assertNoMove), so the recorder can never regress into moving a card.
//
// Which fixture proves which behavior (each is exercised — no vacuous case):
//   - opened-first-observation  → NULL bootstrap records the first observation, cancels
//     nothing (no rework was ever gated in for a NULL mr_state).
//   - opened->merged            → the merged non-edge cancels an in-flight rework exactly
//     once (issue #853) then records 'merged'.
//   - opened->closed            → the close edge cancels exactly once then records 'closed'.
//   - opened->locked            → locked is transient: record, NO cancel.
//   - locked->opened            → the transient state settles back to opened: record, NO cancel.
//   - locked->merged            → merge from the transient state still cancels exactly once.
//   - locked->closed            → a TERMINAL edge: cancels exactly once then records 'closed',
//     matching the issue-lane syncOneMRState — both lanes cancel on any transition into
//     closed/merged from a non-terminal stored state (issue #1072 closed the former parity gap).
//   - unknown-state             → an unrecognized forge state writes nothing and cancels
//     nothing (the baseline is preserved so a later real state still fires).
//   - no-transition             → observed==stored: no write, no cancel.
//   - cancel-failure            → a cancel FAILURE leaves mr_state unadvanced (no record),
//     with the cancel counted once, so the next tick re-observes the edge and retries.
//
// TestR1SelfImproveClosedNoBoardMove is a separate guard on the OTHER watcher
// (SyncMRStates): a self_improve run whose stored mr_state='closed' with a cached open
// tracking issue produces zero board moves, proving the R1 entanglement is benign.

func strptr(s string) *string { return &s }

func TestSyncScheduledMRStatesDifferential(t *testing.T) {
	const mrIID = int64(13)

	cases := []struct {
		name       string
		stored     *string // nil → NULL mr_state (bootstrap)
		observed   string  // what the forge reports live
		cancelErr  bool    // force the rework cancel to fail
		wantRecord *string // nil → assertNoRecord; else the recorded value
		wantCancel int     // expected CancelReworkForMR call count
	}{
		{
			name:       "opened-first-observation",
			stored:     nil,
			observed:   forge.MRStateOpened,
			wantRecord: strptr(forge.MRStateOpened),
			wantCancel: 0,
		},
		{
			name:       "opened->merged",
			stored:     strptr(forge.MRStateOpened),
			observed:   forge.MRStateMerged,
			wantRecord: strptr(forge.MRStateMerged),
			wantCancel: 1,
		},
		{
			name:       "opened->closed",
			stored:     strptr(forge.MRStateOpened),
			observed:   forge.MRStateClosed,
			wantRecord: strptr(forge.MRStateClosed),
			wantCancel: 1,
		},
		{
			name:       "opened->locked",
			stored:     strptr(forge.MRStateOpened),
			observed:   forge.MRStateLocked,
			wantRecord: strptr(forge.MRStateLocked),
			wantCancel: 0,
		},
		{
			name:       "locked->opened",
			stored:     strptr(forge.MRStateLocked),
			observed:   forge.MRStateOpened,
			wantRecord: strptr(forge.MRStateOpened),
			wantCancel: 0,
		},
		{
			name:       "locked->merged",
			stored:     strptr(forge.MRStateLocked),
			observed:   forge.MRStateMerged,
			wantRecord: strptr(forge.MRStateMerged),
			wantCancel: 1,
		},
		{
			// locked->closed is a TERMINAL edge, so it MUST cancel an in-flight rework
			// (issue #853): a confirmed close means the rework can never land. The earlier
			// version pinned wantCancel:0 (close cancelled only from 'opened'), which leaked
			// the rework whenever the MR passed through 'locked' before closing — the run
			// then self-evicted at mr_state='closed' with the rework still spending. Fixed by
			// cancelling on any transition INTO closed/merged from a valid non-terminal state.
			// (The issue-lane syncOneMRState carried the analogous latent leak; fixed in
			// issue #1072 so both lanes now cancel on a locked->closed edge.)
			name:       "locked->closed",
			stored:     strptr(forge.MRStateLocked),
			observed:   forge.MRStateClosed,
			wantRecord: strptr(forge.MRStateClosed),
			wantCancel: 1,
		},
		{
			name:       "unknown-state",
			stored:     strptr(forge.MRStateOpened),
			observed:   "weird",
			wantRecord: nil,
			wantCancel: 0,
		},
		{
			name:       "no-transition",
			stored:     strptr(forge.MRStateOpened),
			observed:   forge.MRStateOpened,
			wantRecord: nil,
			wantCancel: 0,
		},
		{
			name:       "cancel-failure leaves unadvanced",
			stored:     strptr(forge.MRStateOpened),
			observed:   forge.MRStateMerged,
			cancelErr:  true,
			wantRecord: nil, // the retry contract: mr_state stays unadvanced
			wantCancel: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID, repoID := uuid.New(), uuid.New()
			st := &fakeStore{
				scheduledCandidates: []store.ListScheduledMRStateWatchCandidatesRow{
					scheduledCand(runID, mrIID, tc.stored),
				},
			}
			f := &fakeForge{mr: forgeMR(mrIID, tc.observed)}
			canceller := &fakeReworkCanceller{}
			if tc.cancelErr {
				canceller.err = errors.New("cancel failed")
			}
			svc := newTestService(st)
			svc.SetReworkCanceller(canceller)

			if err := svc.SyncScheduledMRStates(context.Background(), repoID, testProjectID, f); err != nil {
				t.Fatalf("SyncScheduledMRStates: %v", err)
			}

			// The recorder NEVER moves a board card.
			assertNoMove(t, f)

			// Recorded state.
			if tc.wantRecord == nil {
				assertNoRecord(t, st)
			} else {
				assertRecorded(t, st, runID, *tc.wantRecord)
			}

			// Cancel-call count (and identity, when it fired).
			if len(canceller.calls) != tc.wantCancel {
				t.Fatalf("CancelReworkForMR calls = %d, want %d: %v", len(canceller.calls), tc.wantCancel, canceller.calls)
			}
			for _, got := range canceller.calls {
				if got != mrIID {
					t.Fatalf("cancelled mrIID = %d, want %d", got, mrIID)
				}
			}

			// Exactly one MR read per candidate.
			if len(f.mrCalls) != 1 || f.mrCalls[0] != mrIID {
				t.Fatalf("expected one GetMergeRequest(%d), got %v", mrIID, f.mrCalls)
			}
		})
	}
}

// TestR1SelfImproveClosedNoBoardMove is a defense-in-depth unit check on the OTHER watcher
// (SyncMRStates, not the board-free recorder). The PRIMARY R1 guard is at the query level:
// ListMRWatchCandidates now excludes kind IN ('prompt','self_improve') in its CTE, so a
// self_improve run is never fed to syncOneMRState in production at all — proven live by
// TestListMRWatchCandidatesLiveDB case 112 (a completed self_improve run whose OPEN tracking
// issue is parked in Human Review with an open MR is NOT a candidate). That query exclusion,
// not a no-transition, is what makes R1 benign — an earlier version of this test asserted the
// opposite (that a cached, Human-Review-promoted tracking issue was safe merely because
// observed==stored), which was vacuous for the dangerous opened->closed edge that WOULD have
// moved the shared card. This test remains only to pin that even if a self_improve candidate
// were somehow handed to syncOneMRState directly, a closed->closed no-transition plans no move.
func TestR1SelfImproveClosedNoBoardMove(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("closed"))},
		// The shared self_improve tracking issue, cached and OPEN.
		issue:   mrIssue(repoID, 9, "opened", board.ColumnHumanReview),
		columns: mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "closed")}

	run(t, st, f)

	assertNoMove(t, f)    // observed=="closed"==stored → no transition, no card move
	assertNoRecord(t, st) // and nothing to record either
}
