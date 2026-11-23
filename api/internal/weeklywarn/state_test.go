package weeklywarn

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// t0 is a fixed instant; every scenario is expressed in hours past it. The week boundary T of a window
// is epoch(t0) + hours*3600.
var t0 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func at(h float64) time.Time { return t0.Add(time.Duration(h * float64(time.Hour))) }

// cw builds a weekly window of bucket/slot at pct resetting at hour resetH past t0 (nil reset when
// noReset).
func cw(bucket, slot string, pct, resetH float64) usagereserve.CodexWeeklyWindow {
	r := at(resetH)
	return usagereserve.CodexWeeklyWindow{BucketID: bucket, Slot: slot, Pct: pct, ResetsAt: &r}
}

func cwNoReset(bucket, slot string, pct float64) usagereserve.CodexWeeklyWindow {
	return usagereserve.CodexWeeklyWindow{BucketID: bucket, Slot: slot, Pct: pct}
}

// sim drives the pure state machines the way the Warner does: one observation then, when the claim
// conditions hold, the claim (warned flips true and a DM is counted).
type sim struct {
	t      *testing.T
	row    Row
	dms    int
	margin int
	limit  *int16
}

func newSim(t *testing.T) *sim { return &sim{t: t, margin: 15} }

func (s *sim) apply(n Next) {
	s.row = Row{AnchorReset: n.AnchorReset, AnchorPct: n.AnchorPct, AnchorBucket: n.AnchorBucket, AnchorMulti: n.AnchorMulti, Warned: n.Warned}
}

func (s *sim) claim(n Next, obs time.Time, w usagereserve.WeeklyReading) bool {
	near := usagereserve.EvaluateNear(s.limit, s.margin, w)
	if Claimable(n, obs, near, s.margin) {
		s.row.Warned = true
		s.dms++
		return true
	}
	return false
}

func (s *sim) codex(obs time.Time, dup map[string]bool, ws ...usagereserve.CodexWeeklyWindow) Next {
	s.t.Helper()
	v := CodexView{Windows: ws, Dup: dup}
	n := nextCodex(s.row, obs, v)
	s.apply(n)
	if trig, ok := v.Trigger(); ok {
		s.claim(n, obs, trig)
	}
	return n
}

func (s *sim) claude(obs time.Time, pct float64, resetH *float64) Next {
	s.t.Helper()
	var reset *time.Time
	if resetH != nil {
		r := at(*resetH)
		reset = &r
	}
	n := nextClaude(s.row, obs, ClaudeWindow{Reset: reset, Pct: pct})
	s.apply(n)
	s.claim(n, obs, usagereserve.WeeklyReading{OK: true, Pct: pct, ResetsAt: reset})
	return n
}

func hp(h float64) *float64 { return &h }

func (s *sim) wantDMs(n int, why string) {
	s.t.Helper()
	if s.dms != n {
		s.t.Fatalf("%s: dms = %d, want %d (row %+v)", why, s.dms, n, s.row)
	}
}

func (s *sim) wantAnchor(bucket string, resetH float64, why string) {
	s.t.Helper()
	if s.row.AnchorReset == nil || s.row.AnchorBucket == nil || *s.row.AnchorBucket != bucket || !s.row.AnchorReset.Equal(at(resetH)) {
		s.t.Fatalf("%s: anchor = (%v, %v), want (%s, %v)", why, s.row.AnchorBucket, s.row.AnchorReset, bucket, at(resetH))
	}
}

func (s *sim) wantNoAnchor(why string) {
	s.t.Helper()
	if s.row.AnchorReset != nil || s.row.AnchorBucket != nil || s.row.AnchorPct != nil || s.row.AnchorMulti || s.row.Warned {
		s.t.Fatalf("%s: row = %+v, want an empty unwarned anchor", why, s.row)
	}
}

const (
	pri = "primary"
	sec = "secondary"
	bkt = "codex"
)

func TestCodexSameWindowWarnsOnce(t *testing.T) {
	s := newSim(t)
	s.codex(at(1), nil, cw(bkt, pri, 40, 100))
	s.wantDMs(0, "below the margin")
	s.codex(at(2), nil, cw(bkt, pri, 90, 100))
	s.wantDMs(1, "first near reading")
	s.codex(at(3), nil, cw(bkt, pri, 95, 100))
	s.codex(at(4), nil, cw(bkt, pri, 100, 100))
	s.wantDMs(1, "same window stays warned")
}

func TestCodexSlotMovement(t *testing.T) {
	s := newSim(t)
	s.codex(at(1), nil, cw(bkt, pri, 90, 100))
	s.wantDMs(1, "warn")
	n := s.codex(at(2), nil, cw(bkt, sec, 93, 100)) // the anchored window moved primary to secondary
	if n.Warned != true {
		t.Fatalf("slot movement rearmed: %+v", n)
	}
	s.wantDMs(1, "slot movement")
	s.wantAnchor(bkt, 100, "anchor after slot movement")
}

// permutations returns every ordering of ws.
func permutations(ws []usagereserve.CodexWeeklyWindow) [][]usagereserve.CodexWeeklyWindow {
	if len(ws) <= 1 {
		return [][]usagereserve.CodexWeeklyWindow{append([]usagereserve.CodexWeeklyWindow(nil), ws...)}
	}
	var out [][]usagereserve.CodexWeeklyWindow
	for i := range ws {
		rest := append(append([]usagereserve.CodexWeeklyWindow(nil), ws[:i]...), ws[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]usagereserve.CodexWeeklyWindow{ws[i]}, p...))
		}
	}
	return out
}

func TestCodexPayloadReorderingIsIdentical(t *testing.T) {
	ws := []usagereserve.CodexWeeklyWindow{
		cw(bkt, pri, 90, 100), cw(bkt, sec, 90, 100.2), cw("gpt", pri, 90, 60), cw("gpt", sec, 20, 120),
	}
	priors := []Row{
		{}, // no anchor
		{AnchorReset: ptrT(at(100)), AnchorPct: ptrF(90), AnchorBucket: strPtr(bkt)},                      // unwarned
		{AnchorReset: ptrT(at(100)), AnchorPct: ptrF(90), AnchorBucket: strPtr(bkt), Warned: true},        // warned
		{AnchorReset: ptrT(at(50)), AnchorPct: ptrF(90), AnchorBucket: strPtr("gpt"), Warned: true},       // anchor far from every window
		{AnchorReset: ptrT(at(200)), AnchorPct: ptrF(90), AnchorBucket: strPtr("gpt"), AnchorMulti: true}, // later anchor
	}
	for i, prev := range priors {
		want := nextCodex(prev, at(10), CodexView{Windows: ws})
		wantTrig, _ := CodexView{Windows: ws}.Trigger()
		for _, p := range permutations(ws) {
			v := CodexView{Windows: p}
			got := nextCodex(prev, at(10), v)
			trig, _ := v.Trigger()
			if !sameNext(got, want) || trig.Pct != wantTrig.Pct || !trig.ResetsAt.Equal(*wantTrig.ResetsAt) {
				t.Fatalf("prior %d: permuted payload changed the verdict: %+v vs %+v (trigger %+v vs %+v)", i, got, want, trig, wantTrig)
			}
		}
	}
}

func ptrT(t time.Time) *time.Time { return &t }

func sameNext(a, b Next) bool {
	eq := func(x, y *time.Time) bool { return (x == nil) == (y == nil) && (x == nil || x.Equal(*y)) }
	fe := func(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	se := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return eq(a.AnchorReset, b.AnchorReset) && fe(a.AnchorPct, b.AnchorPct) && se(a.AnchorBucket, b.AnchorBucket) &&
		a.AnchorMulti == b.AnchorMulti && a.Warned == b.Warned && a.PeriodOK == b.PeriodOK
}

func TestCodexTemporaryAbsenceReturnsBeforeBoundary(t *testing.T) {
	s := newSim(t)
	s.codex(at(1), nil, cw(bkt, pri, 90, 100))
	s.wantDMs(1, "warn")
	// The anchored window is missing; only another bucket's window is reported (near its own limit).
	s.codex(at(2), nil, cw("gpt", pri, 97, 160))
	if !s.row.Warned {
		t.Fatalf("temporary absence rearmed: %+v", s.row)
	}
	s.wantAnchor(bkt, 100, "anchor survives the absence")
	s.codex(at(3), nil, cw(bkt, pri, 92, 100))
	s.wantDMs(1, "the window returns")
	s.wantAnchor(bkt, 100, "anchor after the return")
}

func TestCodexTemporaryAbsencePlusStaggeredWindowDoesNotRoll(t *testing.T) {
	s := newSim(t)
	// A two-window bucket: A resets at 100, B at 148 (two days later).
	s.codex(at(1), nil, cw(bkt, pri, 90, 100), cw(bkt, sec, 40, 148))
	s.wantDMs(1, "warn on A")
	if !s.row.AnchorMulti {
		t.Fatalf("a two-window bucket must set anchor_multi: %+v", s.row)
	}
	// A is missing; B alone, near its limit, before A's boundary.
	s.codex(at(2), nil, cw(bkt, sec, 96, 148))
	s.wantDMs(1, "B alone must not look like a forward roll")
	s.wantAnchor(bkt, 100, "anchor unchanged")
	if !s.row.Warned {
		t.Fatalf("rearmed on a missing sibling: %+v", s.row)
	}
}

func TestCodexPermanentRetirementThenOldBoundaryPasses(t *testing.T) {
	s := newSim(t)
	s.codex(at(1), nil, cw(bkt, pri, 90, 100))
	s.wantDMs(1, "warn")
	// The anchored window is gone for good; another bucket reports 96%. Nothing before the boundary.
	s.codex(at(50), nil, cw("gpt", pri, 96, 300))
	s.wantDMs(1, "before the old boundary")
	// After the old boundary a fresh valid reading re-anchors on the current highest window.
	s.codex(at(101), nil, cw("gpt", pri, 96, 300))
	s.wantDMs(2, "after the old boundary")
	s.wantAnchor("gpt", 300, "re-anchor on the highest window")
	if s.row.Warned != true {
		t.Fatalf("expected the new period to be claimed: %+v", s.row)
	}
	s.codex(at(102), nil, cw("gpt", pri, 97, 300))
	s.wantDMs(2, "once per new period")
}

func TestCodexStaggeredWindowRollingEarlyDoesNotRearm(t *testing.T) {
	s := newSim(t)
	s.codex(at(1), nil, cw(bkt, pri, 90, 100), cw("gpt", pri, 20, 60))
	s.wantDMs(1, "warn on A")
	// Another bucket's window rolls forward (60 -> 228) before A's boundary.
	s.codex(at(70), nil, cw(bkt, pri, 91, 100), cw("gpt", pri, 99, 228))
	s.wantDMs(1, "another window's roll")
	s.wantAnchor(bkt, 100, "anchor unchanged")
}

func TestCodexGenuineUnchangedBoundaryRefill(t *testing.T) {
	s := newSim(t)
	s.codex(at(1), nil, cw(bkt, pri, 90, 100))
	s.wantDMs(1, "warn")
	// The meter zeroes at an unchanged boundary; a relative reset jitters by seconds.
	jitter := usagereserve.CodexWeeklyWindow{BucketID: bkt, Slot: pri, Pct: 0, ResetsAt: ptrT(at(100).Add(7 * time.Second))}
	s.codex(at(2), nil, jitter)
	if s.row.Warned {
		t.Fatalf("a refill must rearm: %+v", s.row)
	}
	s.wantAnchor(bkt, 100, "boundary kept")
	jitter2 := usagereserve.CodexWeeklyWindow{BucketID: bkt, Slot: pri, Pct: 90, ResetsAt: ptrT(at(100).Add(-9 * time.Second))}
	s.codex(at(3), nil, jitter2)
	s.wantDMs(2, "after the refill")
	s.codex(at(4), nil, cw(bkt, pri, 95, 100))
	s.wantDMs(2, "exactly one new DM")
}

func TestCodexAmbiguousCandidatesRefillOnlyIfBothShowIt(t *testing.T) {
	s := newSim(t)
	// Two same-bucket windows, both within the margin of the anchor.
	s.codex(at(1), nil, cw(bkt, pri, 90, 100), cw(bkt, sec, 85, 100.25))
	s.wantDMs(1, "warn")
	s.codex(at(2), nil, cw(bkt, pri, 0, 100), cw(bkt, sec, 80, 100.25))
	if !s.row.Warned {
		t.Fatalf("one candidate cleared and one did not: must not rearm: %+v", s.row)
	}
	s.codex(at(3), nil, cw(bkt, pri, 0, 100), cw(bkt, sec, 0, 100.25))
	if s.row.Warned {
		t.Fatalf("both candidates cleared: must rearm: %+v", s.row)
	}
	if s.row.AnchorPct == nil || *s.row.AnchorPct != 0 {
		t.Fatalf("anchor_pct = %v, want the highest candidate pct (0)", s.row.AnchorPct)
	}
}

func TestCodexDuplicateBucketIDsAreAbsent(t *testing.T) {
	s := newSim(t)
	s.codex(at(1), nil, cw(bkt, pri, 90, 100))
	s.wantDMs(1, "warn")
	dup := map[string]bool{bkt: true}
	// Two buckets share the anchor's id; each carries a cleared window at the boundary. A refill
	// must not be inferred, and a forward roll is disabled.
	s.codex(at(2), dup, cw(bkt, pri, 0, 100), cw(bkt, pri, 0, 100))
	if !s.row.Warned {
		t.Fatalf("duplicate bucket ids matched: %+v", s.row)
	}
	if !s.row.AnchorMulti {
		t.Fatalf("a duplicate bucket id must OR anchor_multi: %+v", s.row)
	}
	s.codex(at(3), dup, cw(bkt, pri, 99, 300))
	s.wantDMs(1, "a duplicate bucket cannot forward-roll")
	s.wantAnchor(bkt, 100, "anchor unchanged")
}

func TestCodexPostBoundaryWithNoFutureWindowClearsAnchor(t *testing.T) {
	s := newSim(t)
	s.codex(at(1), nil, cw(bkt, pri, 90, 100))
	s.wantDMs(1, "warn")
	// After the boundary every window's reset is at or before the observation instant.
	s.codex(at(101), nil, cw("gpt", pri, 95, 100.5))
	s.wantNoAnchor("obsolete anchor cleared")
	s.wantDMs(1, "nothing claimed with no anchor")
	// A later reading re-forms the anchor and can warn.
	s.codex(at(102), nil, cw(bkt, pri, 90, 270))
	s.wantDMs(2, "re-formed anchor warns")
}

func TestCodexLateAppearingSecondWindow(t *testing.T) {
	s := newSim(t)
	// Anchor and warn with A(T) alone: anchor_multi false.
	s.codex(at(1), nil, cw(bkt, pri, 90, 100))
	s.wantDMs(1, "warn on A alone")
	if s.row.AnchorMulti {
		t.Fatalf("a single window must leave anchor_multi false: %+v", s.row)
	}
	// A(T) + B(T+2d): anchor_multi becomes true, no rearm, no DM.
	s.codex(at(2), nil, cw(bkt, pri, 90, 100), cw(bkt, sec, 30, 148))
	if !s.row.AnchorMulti || !s.row.Warned {
		t.Fatalf("second window: want multi true and still warned, got %+v", s.row)
	}
	s.wantDMs(1, "second window appears")
	// Only B(T+2d), near its limit, before T: the forward roll is disabled and the boundary is not due.
	s.codex(at(3), nil, cw(bkt, sec, 97, 148))
	s.wantDMs(1, "B alone while now < T")
	s.wantAnchor(bkt, 100, "anchor unchanged")
	if !s.row.AnchorMulti || !s.row.Warned {
		t.Fatalf("B alone: %+v", s.row)
	}
	// A(T) returns: it matches, no rearm, anchor_multi stays true.
	s.codex(at(4), nil, cw(bkt, pri, 91, 100), cw(bkt, sec, 97, 148))
	s.wantDMs(1, "A returns")
	if !s.row.AnchorMulti || !s.row.Warned {
		t.Fatalf("A returns: %+v", s.row)
	}
	// After T, A is gone (or rolled): the verified post-boundary re-anchor rearms once.
	s.codex(at(101), nil, cw(bkt, sec, 97, 148))
	s.wantDMs(2, "post-boundary re-anchor")
	s.wantAnchor(bkt, 148, "re-anchored on B")
	if s.row.AnchorMulti {
		t.Fatalf("anchor_multi must be recomputed from the reading (one window): %+v", s.row)
	}
	s.codex(at(102), nil, cw(bkt, sec, 98, 148))
	s.wantDMs(2, "at most one DM in the new period")
}

func TestCodexStickyMultiSurvivesAVanishedWindow(t *testing.T) {
	s := newSim(t)
	s.codex(at(1), nil, cw(bkt, pri, 90, 100), cw(bkt, sec, 20, 148))
	if !s.row.AnchorMulti {
		t.Fatalf("multi not set: %+v", s.row)
	}
	// A reading with only one window never clears it within the period.
	s.codex(at(2), nil, cw(bkt, pri, 91, 100))
	s.codex(at(3), nil, cw(bkt, pri, 92, 100))
	if !s.row.AnchorMulti {
		t.Fatalf("anchor_multi cleared by a vanished window: %+v", s.row)
	}
	// The flag also ORs while warned, from a window that appears later.
	s2 := newSim(t)
	s2.codex(at(1), nil, cw(bkt, pri, 90, 100))
	s2.codex(at(2), nil, cw(bkt, pri, 90, 100), cwNoReset(bkt, sec, 10)) // a second window with no usable reset still counts
	if !s2.row.AnchorMulti {
		t.Fatalf("a reset-less second window must count toward anchor_multi: %+v", s2.row)
	}
}

func TestCodexForwardRollRearmsWhenBucketNeverShowedASecondWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		nextH  float64
		obsH   float64
		wantDM int
	}{
		{"roll more than the margin past the anchor", 100 + 24, 10, 2},
		{"roll of one hour and a bit", 100 + 1.5, 10, 2},
		{"within the margin is the same window", 100 + 0.5, 10, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSim(t)
			s.codex(at(1), nil, cw(bkt, pri, 90, 100))
			s.wantDMs(1, "warn")
			s.codex(at(tc.obsH), nil, cw(bkt, pri, 5, tc.nextH))
			s.codex(at(tc.obsH+1), nil, cw(bkt, pri, 92, tc.nextH))
			s.wantDMs(tc.wantDM, tc.name)
		})
	}
}

func TestCodexMultiResetRulesOnUnwarnedReform(t *testing.T) {
	// An unwarned re-form in the same bucket ORs the flag; a re-form to another bucket resets it.
	s := newSim(t)
	s.codex(at(1), nil, cw(bkt, pri, 40, 100), cw(bkt, sec, 10, 148))
	if !s.row.AnchorMulti {
		t.Fatalf("multi: %+v", s.row)
	}
	s.codex(at(2), nil, cw(bkt, pri, 50, 148))
	s.wantAnchor(bkt, 148, "same bucket, later window")
	if !s.row.AnchorMulti {
		t.Fatalf("same-bucket re-form must OR the flag: %+v", s.row)
	}
	s.codex(at(3), nil, cw(bkt, pri, 50, 148), cw("gpt", pri, 60, 150))
	s.wantAnchor("gpt", 150, "re-form to another bucket")
	if s.row.AnchorMulti {
		t.Fatalf("a re-form to another bucket must reset the flag from that reading: %+v", s.row)
	}
}

func TestCodexReanchorTieBreaks(t *testing.T) {
	// Equal utilization: earliest reset, then the smallest bucket id, then primary before secondary.
	v := CodexView{Windows: []usagereserve.CodexWeeklyWindow{
		cw("b", sec, 50, 100), cw("a", sec, 50, 100), cw("a", pri, 50, 100), cw("z", pri, 50, 90),
	}}
	if c := v.choose(at(0).Unix(), nil); c.BucketID != "z" {
		t.Fatalf("earliest reset not preferred: %+v", c)
	}
	v.Windows = v.Windows[:3]
	if c := v.choose(at(0).Unix(), nil); c.BucketID != "a" || c.Slot != pri {
		t.Fatalf("bucket id then primary not preferred: %+v", c)
	}
	// A window whose reset is not after the observation, or earlier than the floor, is skipped.
	floor := at(95).Unix()
	v = CodexView{Windows: []usagereserve.CodexWeeklyWindow{cw("a", pri, 90, 90), cw("b", pri, 10, 100), cw("c", pri, 99, 5)}}
	if c := v.choose(at(6).Unix(), &floor); c == nil || c.BucketID != "b" {
		t.Fatalf("floor/obs filters wrong: %+v", c)
	}
}

func TestCodexNeverReanchorsEarlierThanAnchorMinusMargin(t *testing.T) {
	s := newSim(t)
	s.margin = 0 // only the anchor is under test, not the claim
	s.codex(at(1), nil, cw(bkt, pri, 40, 100))
	// A replayed old window (reset at hour 90, ten hours before the anchor) at 99% is not followed.
	s.codex(at(2), nil, cw(bkt, pri, 40, 100), cw("gpt", pri, 99, 90))
	s.wantAnchor(bkt, 100, "replayed window ignored")
	// A window within the margin below the anchor is still followed.
	s.codex(at(3), nil, cw(bkt, pri, 40, 99.5))
	s.wantAnchor(bkt, 99.5, "within the margin")
}

func TestCodexFirstAnchorAndNullAnchor(t *testing.T) {
	s := newSim(t)
	s.codex(at(1), nil, cw(bkt, pri, 10, 100), cw("gpt", pri, 30, 0.5))
	s.wantAnchor(bkt, 100, "windows with a past reset never anchor")
	s2 := newSim(t)
	n := s2.codex(at(1), nil, cwNoReset(bkt, pri, 99))
	if n.AnchorReset != nil || s2.dms != 0 {
		t.Fatalf("a reset-less window anchors nothing and sends nothing: %+v dms=%d", n, s2.dms)
	}
}

func TestCodexRelativeZeroResetNeverWarns(t *testing.T) {
	s := newSim(t)
	// A relative-zero reset equals the observation instant: not strictly after it.
	s.codex(at(1), nil, usagereserve.CodexWeeklyWindow{BucketID: bkt, Slot: pri, Pct: 99, ResetsAt: ptrT(at(1))})
	s.wantDMs(0, "relative zero")
}

func TestCodexTriggerRequiresResetAfterObservation(t *testing.T) {
	s := newSim(t)
	// The highest window (99%) has no usable reset; a lower window (50%) has one. The near verdict is
	// read from the highest, whose reset is unusable: nothing is sent.
	s.codex(at(1), nil, cwNoReset(bkt, pri, 99), cw("gpt", pri, 50, 100))
	s.wantDMs(0, "triggering window without a reset")
}

// ---- Claude ----

func TestClaudeStateMachine(t *testing.T) {
	t.Run("same window warns once", func(t *testing.T) {
		s := newSim(t)
		s.claude(at(1), 40, hp(100))
		s.claude(at(2), 90, hp(100))
		s.wantDMs(1, "first near")
		s.claude(at(3), 99, hp(100))
		s.wantDMs(1, "same window")
	})
	t.Run("in-window jitter updates the anchor and does not rearm", func(t *testing.T) {
		s := newSim(t)
		s.claude(at(1), 90, hp(100))
		s.claude(at(2), 91, hp(100.4))
		if s.row.AnchorReset == nil || !s.row.AnchorReset.Equal(at(100.4)) || *s.row.AnchorPct != 91 || !s.row.Warned {
			t.Fatalf("in-window update: %+v", s.row)
		}
		s.wantDMs(1, "jitter")
	})
	t.Run("a 1h to 1d forward reset is a new period", func(t *testing.T) {
		for _, h := range []float64{1.5, 12, 24} {
			s := newSim(t)
			s.claude(at(1), 90, hp(100))
			s.claude(at(2), 91, hp(100+h))
			s.wantDMs(2, "forward reset")
		}
	})
	t.Run("a forward move within the margin is the same window", func(t *testing.T) {
		s := newSim(t)
		s.claude(at(1), 90, hp(100))
		s.claude(at(2), 91, hp(100.9))
		s.wantDMs(1, "within the margin")
	})
	t.Run("90 then 0 then 90 at the boundary", func(t *testing.T) {
		s := newSim(t)
		s.claude(at(1), 90, hp(100))
		s.wantDMs(1, "warn")
		s.claude(at(2), 0, hp(100))
		if s.row.Warned {
			t.Fatalf("arm B must rearm: %+v", s.row)
		}
		s.claude(at(3), 90, hp(100))
		s.wantDMs(2, "second warning after the refill")
	})
	t.Run("a lagging post-boundary reading at 92 percent does not rearm", func(t *testing.T) {
		s := newSim(t)
		s.claude(at(1), 90, hp(100))
		s.wantDMs(1, "warn")
		// The boundary has passed (obs 101 > 100) but the provider still reports the old reset.
		s.claude(at(101), 92, hp(100))
		s.wantDMs(1, "lagging reading")
		if !s.row.Warned {
			t.Fatalf("lagging reading rearmed: %+v", s.row)
		}
		// The later real new reset: exactly one rearm.
		s.claude(at(102), 93, hp(268))
		s.wantDMs(2, "the real new reset")
		s.claude(at(103), 94, hp(268))
		s.wantDMs(2, "once per new period")
	})
	t.Run("post-boundary with no reset clears the anchor", func(t *testing.T) {
		s := newSim(t)
		s.claude(at(1), 90, hp(100))
		s.claude(at(101), 10, nil)
		s.wantNoAnchor("nil reset after the boundary")
		s.claude(at(102), 90, hp(268))
		s.wantDMs(2, "re-formed")
	})
	t.Run("post-boundary with a reset not after the observation clears the anchor", func(t *testing.T) {
		s := newSim(t)
		s.claude(at(1), 90, hp(100))
		s.claude(at(101), 10, hp(50)) // reset before anchor-margin and not in the future
		s.wantNoAnchor("obsolete reset")
	})
	t.Run("a replayed older reset before the boundary changes nothing", func(t *testing.T) {
		s := newSim(t)
		s.claude(at(1), 90, hp(100))
		s.claude(at(2), 91, hp(50))
		if s.row.AnchorReset == nil || !s.row.AnchorReset.Equal(at(100)) || !s.row.Warned {
			t.Fatalf("replay moved the anchor: %+v", s.row)
		}
		s.wantDMs(1, "replay")
	})
	t.Run("no reset forms no anchor and sends nothing", func(t *testing.T) {
		s := newSim(t)
		s.claude(at(1), 99, nil)
		s.wantNoAnchor("no reset")
		s.wantDMs(0, "no reset")
	})
	t.Run("the claim requires the reset after the observation", func(t *testing.T) {
		s := newSim(t)
		s.claude(at(1), 99, hp(1)) // relative-zero equivalent: reset == obs
		s.wantDMs(0, "reset not after the observation")
	})
}

// ---- claim conditions ----

func TestClaimableConditions(t *testing.T) {
	ok := Next{PeriodOK: true}
	reset := at(100)
	near := usagereserve.NearVerdict{State: usagereserve.NearNear, ResetsAt: &reset}
	if !Claimable(ok, at(1), near, 15) {
		t.Fatal("baseline must claim")
	}
	for name, got := range map[string]bool{
		"warned":       Claimable(Next{PeriodOK: true, Warned: true}, at(1), near, 15),
		"period gate":  Claimable(Next{}, at(1), near, 15),
		"margin 0":     Claimable(ok, at(1), near, 0),
		"below":        Claimable(ok, at(1), usagereserve.NearVerdict{State: usagereserve.NearBelow, ResetsAt: &reset}, 15),
		"unknown":      Claimable(ok, at(1), usagereserve.NearVerdict{State: usagereserve.NearUnknown, ResetsAt: &reset}, 15),
		"no reset":     Claimable(ok, at(1), usagereserve.NearVerdict{State: usagereserve.NearNear}, 15),
		"reset == obs": Claimable(ok, at(100), near, 15),
		"reset before": Claimable(ok, at(101), near, 15),
	} {
		if got {
			t.Errorf("%s: claimed", name)
		}
	}
	if !Claimable(ok, at(1), usagereserve.NearVerdict{State: usagereserve.NearAt, ResetsAt: &reset}, 15) {
		t.Error("at-limit must claim")
	}
}

func TestMarginIsOneHour(t *testing.T) {
	if marginSecs != 3600 {
		t.Fatalf("margin = %d", marginSecs)
	}
}

// An observation exactly at the anchored reset is not yet past the boundary (strictly after only).
func TestObservationAtTheAnchorExactlyIsNotPostBoundary(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		warned := Row{AnchorReset: ptrT(at(100)), AnchorPct: ptrF(90), Warned: true}
		// No reset in the reading: the post-boundary path would clear the anchor.
		n := nextClaude(warned, at(100), ClaudeWindow{Pct: 50})
		if n.AnchorReset == nil || !n.AnchorReset.Equal(at(100)) || !n.Warned || n.PeriodOK {
			t.Fatalf("obs == anchor must freeze the row: %+v", n)
		}
		// One second later the verified post-boundary path applies.
		n = nextClaude(warned, at(100).Add(time.Second), ClaudeWindow{Pct: 50})
		if n.AnchorReset != nil || n.Warned {
			t.Fatalf("obs just past the anchor must clear it: %+v", n)
		}
	})
	t.Run("codex", func(t *testing.T) {
		warned := Row{AnchorReset: ptrT(at(100)), AnchorPct: ptrF(90), AnchorBucket: strPtr(bkt), Warned: true}
		v := CodexView{Windows: []usagereserve.CodexWeeklyWindow{cw("gpt", pri, 50, 300)}}
		n := nextCodex(warned, at(100), v)
		if n.AnchorBucket == nil || *n.AnchorBucket != bkt || !n.Warned || n.PeriodOK {
			t.Fatalf("obs == anchor must freeze the row: %+v", n)
		}
		n = nextCodex(warned, at(100).Add(time.Second), v)
		if n.AnchorBucket == nil || *n.AnchorBucket != "gpt" || n.Warned {
			t.Fatalf("obs just past the anchor must start a new period: %+v", n)
		}
	})
}

// A refill whose reset jitters by a few seconds is still a refill: warned at 90, 0 at T+3s, then 90
// again gives exactly one new warning.
func TestClaudeRefillUnderResetJitterRearms(t *testing.T) {
	s := newSim(t)
	s.claude(at(1), 90, hp(100))
	s.wantDMs(1, "warn")
	s.claude(at(2), 0, hp(100+3.0/3600))
	if s.row.Warned {
		t.Fatalf("a jittered refill must rearm: %+v", s.row)
	}
	s.claude(at(3), 90, hp(100+3.0/3600))
	s.wantDMs(2, "exactly one new period")
	s.claude(at(4), 95, hp(100+3.0/3600))
	s.wantDMs(2, "once per period")
	// A small drop that is not a refill leaves the row warned and still follows the anchor.
	s2 := newSim(t)
	s2.claude(at(1), 90, hp(100))
	s2.claude(at(2), 70, hp(100+3.0/3600))
	if !s2.row.Warned || *s2.row.AnchorPct != 70 {
		t.Fatalf("a non-refill jitter reading: %+v", s2.row)
	}
}
