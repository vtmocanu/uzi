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
	for want := 1; want <= 3; want++ {
		next, _ := m.Update(boardRunsMsg{err: pollErr})
		m = next.(tuiModel)
		if m.board.errStreak != want {
			t.Fatalf("after %d error replies errStreak = %d, want %d", want, m.board.errStreak, want)
		}
		if got, exp := boardTickInterval(m.board.errStreak), boardTickInterval(want); got != exp {
			t.Fatalf("interval at streak %d = %v, want %v", want, got, exp)
		}
	}
	// The grown interval is strictly larger than the base (backoff actually took effect).
	if boardTickInterval(m.board.errStreak) <= boardPollInterval {
		t.Fatalf("interval at streak %d = %v did not exceed the base %v", m.board.errStreak, boardTickInterval(m.board.errStreak), boardPollInterval)
	}

	// A success reply resets the streak to 0 → interval back to the base.
	next, _ := m.Update(boardRunsMsg{runs: runs})
	m = next.(tuiModel)
	if m.board.errStreak != 0 {
		t.Fatalf("a success reply left errStreak = %d, want 0", m.board.errStreak)
	}
	if got := boardTickInterval(m.board.errStreak); got != boardPollInterval {
		t.Fatalf("after a success the interval = %v, want the base %v", got, boardPollInterval)
	}
}

// The key gating assertion (M3 D4 / mirrors D2): a stale admin-mismatch reply is ignored by apply
// via its early return, so it must leave the error streak UNCHANGED — neither inflating nor
// resetting the backoff.
func TestBoardErrStreakUnchangedOnAdminMismatch(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runs}
	m := tuiTestModel(t, fake, "") // own board: b.admin == false

	// Establish a known non-zero streak with two own-board error replies.
	for i := 0; i < 2; i++ {
		next, _ := m.Update(boardRunsMsg{err: uzicli.Exitf(uzicli.ExitGeneric, "context deadline exceeded")})
		m = next.(tuiModel)
	}
	if m.board.errStreak != 2 {
		t.Fatalf("precondition: errStreak = %d, want 2", m.board.errStreak)
	}
	before := m.board.errStreak

	// A stale admin reply (msg.admin == true while b.admin == false): apply's early return ignores
	// it, so the streak must be identical afterward — a success-shaped admin reply must not reset it,
	// and an error-shaped one must not inflate it.
	next, _ := m.Update(boardRunsMsg{admin: true, runs: runs})
	m = next.(tuiModel)
	if m.board.errStreak != before {
		t.Fatalf("an admin-mismatch success reply changed errStreak from %d to %d; the stale reply must leave it untouched", before, m.board.errStreak)
	}

	next, _ = m.Update(boardRunsMsg{admin: true, err: uzicli.Exitf(uzicli.ExitGeneric, "boom")})
	m = next.(tuiModel)
	if m.board.errStreak != before {
		t.Fatalf("an admin-mismatch error reply changed errStreak from %d to %d; the stale reply must leave it untouched", before, m.board.errStreak)
	}
}
