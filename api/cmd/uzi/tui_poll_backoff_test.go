package main

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1130 M3: after consecutive failed board polls the reschedule interval backs off
// (2s → 4s → 8s → … capped ~30s) so a bad link is not hammered at the full 2s cadence, and the
// FIRST success resets it to base. The mapping is a pure helper (D4) so the interval is
// unit-assertable without inspecting an opaque tea.Tick.

// boardTickInterval maps the consecutive-error streak to a reschedule interval: base at streak 0,
// doubling each consecutive failure, clamped at boardBackoffCap and never exceeding it.
func TestBoardTickIntervalBackoff(t *testing.T) {
	// Streak 0 is exactly the base cadence.
	if got := boardTickInterval(0); got != boardPollInterval {
		t.Fatalf("boardTickInterval(0) = %v, want the base %v", got, boardPollInterval)
	}
	// A negative streak is defensive-treated as the base.
	if got := boardTickInterval(-3); got != boardPollInterval {
		t.Fatalf("boardTickInterval(-3) = %v, want the base %v", got, boardPollInterval)
	}

	// The explicit doubling schedule against the default 2s base / 30s cap.
	cases := []struct {
		streak int
		want   time.Duration
	}{
		{0, 2 * time.Second},
		{1, 4 * time.Second},
		{2, 8 * time.Second},
		{3, 16 * time.Second},
		{4, 30 * time.Second}, // 32s would exceed the cap → clamped
		{5, 30 * time.Second},
		{6, 30 * time.Second},
	}
	for _, c := range cases {
		if got := boardTickInterval(c.streak); got != c.want {
			t.Errorf("boardTickInterval(%d) = %v, want %v", c.streak, got, c.want)
		}
	}

	// Monotonic non-decreasing across 0..6, and strictly increasing until it first reaches the cap.
	reachedCap := false
	prev := boardTickInterval(0)
	for s := 1; s <= 6; s++ {
		cur := boardTickInterval(s)
		if cur < prev {
			t.Fatalf("boardTickInterval decreased: streak %d = %v < streak %d = %v", s, cur, s-1, prev)
		}
		if cur > boardBackoffCap {
			t.Fatalf("boardTickInterval(%d) = %v exceeds the cap %v", s, cur, boardBackoffCap)
		}
		if !reachedCap {
			if cur == boardBackoffCap {
				reachedCap = true
			} else if cur <= prev {
				t.Fatalf("boardTickInterval(%d) = %v did not strictly increase from %v before hitting the cap", s, cur, prev)
			}
		} else if cur != boardBackoffCap {
			t.Fatalf("boardTickInterval(%d) = %v should stay clamped at the cap %v", s, cur, boardBackoffCap)
		}
		prev = cur
	}
	if !reachedCap {
		t.Fatal("boardTickInterval never reached the cap across streak 0..6")
	}

	// A very large streak stays clamped at the cap and never overflows into a huge or negative value.
	if got := boardTickInterval(100); got != boardBackoffCap {
		t.Fatalf("boardTickInterval(100) = %v, want the cap %v", got, boardBackoffCap)
	}
}

// The streak grows on each consecutive error reply and resets on the first success, both driven
// through apply (the message case's boardRunsMsg → apply). Assert the resulting durations, not a
// tally.
func TestBoardErrStreakUpdatesInterval(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runs}
	m := tuiTestModel(t, fake, "")

	pollErr := uzicli.Exitf(uzicli.ExitGeneric, "context deadline exceeded")

	// Three consecutive error replies grow the streak 1, 2, 3 and therefore grow the interval.
	// Each reply carries the outstanding request's id (reqID == waitID), and a tick between them
	// mints the next request so the following reply has a fresh matching id (the seeded Init poll
	// supplies the first).
	for want := 1; want <= 3; want++ {
		next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, err: pollErr})
		m = next.(tuiModel)
		if m.board.errStreak != want {
			t.Fatalf("after %d error replies errStreak = %d, want %d", want, m.board.errStreak, want)
		}
		if got, exp := boardTickInterval(m.board.errStreak), boardTickInterval(want); got != exp {
			t.Fatalf("interval at streak %d = %v, want %v", want, got, exp)
		}
		next, _ = m.Update(boardTickMsg{gen: m.board.tickGen}) // mint the next request
		m = next.(tuiModel)
	}
	// The grown interval is strictly larger than the base (backoff actually took effect).
	if boardTickInterval(m.board.errStreak) <= boardPollInterval {
		t.Fatalf("interval at streak %d = %v did not exceed the base %v", m.board.errStreak, boardTickInterval(m.board.errStreak), boardPollInterval)
	}

	// A success reply resets the streak to 0 → interval back to the base.
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	m = next.(tuiModel)
	if m.board.errStreak != 0 {
		t.Fatalf("a success reply left errStreak = %d, want 0", m.board.errStreak)
	}
	if got := boardTickInterval(m.board.errStreak); got != boardPollInterval {
		t.Fatalf("after a success the interval = %v, want the base %v", got, boardPollInterval)
	}
}

// The key gating assertion in the id-matching model (M3 D4 / mirrors D2): a stale-reqID reply is
// dropped whole (never reaching apply), and a CURRENT-reqID admin-mismatch reply is ignored by
// apply's own early return — both must leave the error streak UNCHANGED, neither inflating nor
// resetting the backoff. The current-reqID admin-mismatch reply still clears the guard.
func TestBoardErrStreakUnchangedOnAdminMismatch(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runs}
	m := tuiTestModel(t, fake, "") // own board: b.admin == false

	pollErr := uzicli.Exitf(uzicli.ExitGeneric, "context deadline exceeded")
	// Establish a known non-zero streak with two own-board error replies (matching reqID), re-arming
	// a fresh request between and after them.
	for i := 0; i < 2; i++ {
		next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, err: pollErr})
		m = next.(tuiModel)
		next, _ = m.Update(boardTickMsg{gen: m.board.tickGen}) // mint the next request
		m = next.(tuiModel)
	}
	if m.board.errStreak != 2 {
		t.Fatalf("precondition: errStreak = %d, want 2", m.board.errStreak)
	}
	before := m.board.errStreak
	waitID := m.board.waitID
	if waitID == 0 {
		t.Fatal("precondition: no request outstanding for the admin-mismatch reply")
	}

	// A CURRENT-reqID admin-mismatch success reply (msg.admin == true while b.admin == false):
	// apply's early return ignores it, so the streak must be identical afterward — a success-shaped
	// admin reply must not reset it — while the reqID-matching case still clears the guard.
	next, _ := m.Update(boardRunsMsg{reqID: waitID, admin: true, runs: runs})
	m = next.(tuiModel)
	if m.board.errStreak != before {
		t.Fatalf("an admin-mismatch success reply changed errStreak from %d to %d; apply must leave it untouched", before, m.board.errStreak)
	}
	if m.board.waitID != 0 {
		t.Fatal("the current-reqID admin-mismatch reply did not clear the guard")
	}

	// A STALE-reqID reply is dropped before apply and likewise leaves the streak untouched. Re-arm
	// so there is an outstanding id to be stale against.
	next, _ = m.Update(boardTickMsg{gen: m.board.tickGen})
	m = next.(tuiModel)
	next, _ = m.Update(boardRunsMsg{reqID: m.board.waitID + 999, admin: true, err: uzicli.Exitf(uzicli.ExitGeneric, "boom")})
	m = next.(tuiModel)
	if m.board.errStreak != before {
		t.Fatalf("a stale-reqID error reply changed errStreak from %d to %d; it must be dropped whole", before, m.board.errStreak)
	}
}

// Finding 1: the FIRST retry after a failed poll must use the BACKED-OFF interval, not the stale
// pre-failure one. The fix moves rescheduling out of the boardTickMsg case and into the reply,
// which re-arms AFTER apply has bumped errStreak — so the reschedule computed at reply time uses
// the fresh streak. This test drives a tick to issue the fetch, delivers a failed reply, and
// proves (a) the reply re-arms the tick carrying the bumped generation, (b) the boardTickMsg case
// itself no longer returns a standalone re-arm tick when it issues a fetch, and (c) the reschedule
// interval is boardTickInterval(1), which exceeds the base.
func TestBoardFirstRetryAfterFailureUsesBackoff(t *testing.T) {
	origInterval := boardPollInterval
	boardPollInterval = time.Millisecond // so boardTickInterval(1) ≈ 2ms doesn't block the drain
	t.Cleanup(func() { boardPollInterval = origInterval })

	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runs}
	m := tuiTestModel(t, fake, "")

	// Bring the board to idle by delivering the Init reply.
	next, _ := m.Update(boardRunsMsg{reqID: 1, runs: runs})
	m = next.(tuiModel)

	// A tick from idle issues the board fetch. Proof rescheduling MOVED to the reply: the tick case
	// returns ONLY the fetch (no standalone re-arm tick) and does NOT bump tickGen itself.
	genAtTick := m.board.tickGen
	next, tickReturn := m.Update(boardTickMsg{gen: m.board.tickGen})
	m = next.(tuiModel)
	waitID := m.board.waitID
	if waitID == 0 {
		t.Fatal("the tick from idle did not issue a board fetch")
	}
	if hasMsg[boardTickMsg](drainCmd(tickReturn)) {
		t.Fatal("the boardTickMsg case re-armed a tick itself when it issued a fetch; rescheduling must live in the reply")
	}
	if m.board.tickGen != genAtTick {
		t.Fatalf("the tick case bumped tickGen (%d → %d); only the reply should reschedule", genAtTick, m.board.tickGen)
	}

	// A FAILED poll reply for that request.
	genBefore := m.board.tickGen
	next, replyCmd := m.Update(boardRunsMsg{reqID: waitID, err: uzicli.Exitf(uzicli.ExitGeneric, "context deadline exceeded")})
	m = next.(tuiModel)
	if m.board.errStreak != 1 {
		t.Fatalf("a failed poll left errStreak = %d, want 1", m.board.errStreak)
	}
	if m.board.tickGen != genBefore+1 {
		t.Fatalf("the reply did not bump tickGen (%d, want %d)", m.board.tickGen, genBefore+1)
	}

	// The reply re-arms the board tick, carrying the BUMPED generation.
	var rearm *boardTickMsg
	for _, msg := range drainCmd(replyCmd) {
		if bt, ok := msg.(boardTickMsg); ok {
			b := bt
			rearm = &b
		}
	}
	if rearm == nil {
		t.Fatal("the failed-poll reply did not re-arm the board tick")
	}
	if rearm.gen != m.board.tickGen {
		t.Fatalf("the re-armed tick carries gen %d, want the bumped %d", rearm.gen, m.board.tickGen)
	}

	// The reschedule computed at reply time uses the FRESH streak: the first retry backs off.
	if got, want := boardTickInterval(m.board.errStreak), boardTickInterval(1); got != want {
		t.Fatalf("first-retry interval = %v, want boardTickInterval(1) = %v", got, want)
	}
	if boardTickInterval(m.board.errStreak) <= boardTickInterval(0) {
		t.Fatalf("first-retry interval %v does not exceed the base %v", boardTickInterval(m.board.errStreak), boardTickInterval(0))
	}
}
