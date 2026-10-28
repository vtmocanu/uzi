package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// Unit tests for the manual-resume override rules and the reserve pause helpers (PRD #3522 M4a).

var (
	ovGrant  = reserveT0.Add(-2 * time.Hour)
	ovAnchor = reserveT0.Add(48 * time.Hour)
)

// ovSnap is a threshold-50 snapshot whose one Claude token and one Codex alias carry the given
// verdict, synced at `synced`.
func ovSnap(v usagereserve.Verdict, synced time.Time) *reserveSnap {
	return &reserveSnap{
		userID: uuid.New(), threshold: i16p(50), pauseRunning: true,
		claude: map[uuid.UUID]usagereserve.Verdict{tokDefault: v},
		codex:  map[uuid.UUID]usagereserve.Verdict{aliasHi: v},
		synced: map[uuid.UUID]time.Time{tokDefault: synced, aliasHi: synced},
	}
}

func freshV(state usagereserve.State, pct int, resets time.Time) usagereserve.Verdict {
	return usagereserve.Verdict{State: state, Pct: pct, Threshold: 50, ResetsAt: &resets, Fresh: true}
}

// ovRun is a queued Claude run holding a manual-resume exemption granted at ovGrant.
func ovRun(harness string, until time.Time, anchor *time.Time) store.Run {
	r := store.Run{
		Kind: runkind.Issue, Harness: harness, Status: "queued",
		ReserveExemptUntil:        pgconv.Time(until),
		ReserveExemptGrantedAt:    pgconv.Time(ovGrant),
		ReserveExemptCredentialID: pgconv.UUID(tokDefault),
	}
	if harness == harnessCodex {
		r.ReserveExemptCredentialID = pgconv.UUID(aliasHi)
	}
	if anchor != nil {
		r.ReserveExemptWindowReset = pgconv.Time(*anchor)
		// A real grant records its anchor only from a fresh held reading, together with that
		// reading's utilization as the reset detector's prior (72% here, above every test threshold).
		r.ReserveExemptPriorPct = pgtype.Int2{Int16: 72, Valid: true}
	}
	return r
}

// TestOverrideActiveReadingAware: the exemption ends at the first of the clock, an observed reset
// (early or on time) against the anchor in a reading synced after the grant, or, with no anchor, the
// first fresh reading below the threshold synced after the grant.
func TestOverrideActiveReadingAware(t *testing.T) {
	now := reserveT0
	anchor := ovAnchor
	afterGrant := ovGrant.Add(time.Hour)
	beforeGrant := ovGrant.Add(-time.Hour)
	moved := anchor.Add(7 * 24 * time.Hour) // a week later: the boundary moved
	withPrior := func(run store.Run, pct int16) store.Run {
		run.ReserveExemptPriorPct = pgtype.Int2{Int16: pct, Valid: true}
		return run
	}
	withThr := func(snap *reserveSnap, thr int16) *reserveSnap {
		snap.threshold = &thr
		return snap
	}
	cases := []struct {
		name   string
		harn   string
		run    store.Run
		snap   *reserveSnap
		now    time.Time
		active bool
	}{
		{"within the window, unchanged reading", "claude", ovRun("claude", anchor, &anchor), ovSnap(freshV(usagereserve.Held, 60, anchor), afterGrant), now, true},
		{"clock reached the anchor", "claude", ovRun("claude", anchor, &anchor), ovSnap(freshV(usagereserve.Held, 60, anchor), afterGrant), anchor, false},
		{"7-day cap with no anchor reached", "claude", ovRun("claude", ovGrant.Add(ReserveOverrideWindow), nil), ovSnap(usagereserve.Verdict{State: usagereserve.Unknown}, afterGrant), ovGrant.Add(ReserveOverrideWindow), false},
		{"early reset: boundary moved, usage already back above the line", "claude", ovRun("claude", anchor, &anchor), ovSnap(freshV(usagereserve.Held, 55, moved), afterGrant), now, false},
		{"early reset seen only in a reading from BEFORE the grant", "claude", ovRun("claude", anchor, &anchor), ovSnap(freshV(usagereserve.Held, 55, moved), beforeGrant), now, true},
		{"boundary jitter within the move margin is not a reset", "claude", ovRun("claude", anchor, &anchor), ovSnap(freshV(usagereserve.Held, 60, anchor.Add(30*time.Minute)), afterGrant), now, true},
		{"fixed-grid reset: boundary unmoved, usage zeroed", "claude", ovRun("claude", anchor, &anchor), ovSnap(freshV(usagereserve.OK, 0, anchor), afterGrant), now, false},
		{"unmoved boundary, usage still high", "claude", ovRun("claude", anchor, &anchor), ovSnap(freshV(usagereserve.Held, 80, anchor), afterGrant), now, true},
		{"a stale (non-fresh) reading ends nothing", "claude", ovRun("claude", anchor, &anchor), ovSnap(usagereserve.Verdict{State: usagereserve.Unknown, ResetsAt: &moved}, afterGrant), now, true},
		{"no anchor: first fresh reading below the threshold ends it", "claude", ovRun("claude", ovGrant.Add(ReserveOverrideWindow), nil), ovSnap(freshV(usagereserve.OK, 20, anchor), afterGrant), now, false},
		{"no anchor: a fresh reading still above the line keeps it", "claude", ovRun("claude", ovGrant.Add(ReserveOverrideWindow), nil), ovSnap(freshV(usagereserve.Held, 70, anchor), afterGrant), now, true},
		{"no anchor: a below-threshold reading from before the grant keeps it", "claude", ovRun("claude", ovGrant.Add(ReserveOverrideWindow), nil), ovSnap(freshV(usagereserve.OK, 20, anchor), beforeGrant), now, true},
		{"codex early reset", "codex", ovRun("codex", anchor, &anchor), ovSnap(freshV(usagereserve.Held, 55, moved), afterGrant), now, false},
		{"codex unmoved boundary", "codex", ovRun("codex", anchor, &anchor), ovSnap(freshV(usagereserve.Held, 55, anchor), afterGrant), now, true},
		{"codex no anchor, fresh below the line", "codex", ovRun("codex", ovGrant.Add(ReserveOverrideWindow), nil), ovSnap(freshV(usagereserve.OK, 5, anchor), afterGrant), now, false},
		{"threshold 1, claude: the same fresh 1% under the unchanged boundary is not a reset", "claude", withPrior(ovRun("claude", anchor, &anchor), 1), withThr(ovSnap(freshV(usagereserve.Held, 1, anchor), afterGrant), 1), now, true},
		{"threshold 1, codex: the same fresh 1.0 under the unchanged boundary is not a reset", "codex", withPrior(ovRun("codex", anchor, &anchor), 1), withThr(ovSnap(freshV(usagereserve.Held, 1, anchor), afterGrant), 1), now, true},
		{"threshold 1: a moved boundary still ends it", "claude", withPrior(ovRun("claude", anchor, &anchor), 1), withThr(ovSnap(freshV(usagereserve.Held, 1, moved), afterGrant), 1), now, false},
		{"threshold 70, fresh 1% under the unchanged boundary is a reset", "claude", ovRun("claude", anchor, &anchor), withThr(ovSnap(freshV(usagereserve.OK, 1, anchor), afterGrant), 70), now, false},
		{"threshold 70, codex fresh 1.0 under the unchanged boundary is a reset", "codex", ovRun("codex", anchor, &anchor), withThr(ovSnap(freshV(usagereserve.OK, 1, anchor), afterGrant), 70), now, false},
		{"no exemption", "claude", store.Run{Kind: runkind.Issue}, ovSnap(freshV(usagereserve.OK, 5, anchor), afterGrant), now, false},
	}
	for _, c := range cases {
		if got := overrideActive(c.run, c.snap, c.now); got != c.active {
			t.Errorf("%s: overrideActive = %v, want %v", c.name, got, c.active)
		}
	}
	// reserveEnforced follows: an ended override is enforced again, an active one is not.
	ended := ovRun("claude", anchor, &anchor)
	if !reserveEnforced(ended, ovSnap(freshV(usagereserve.Held, 55, moved), afterGrant), now) {
		t.Error("an override ended by an early reset must be enforced again")
	}
	if reserveEnforced(ended, ovSnap(freshV(usagereserve.Held, 60, anchor), afterGrant), now) {
		t.Error("an active override must not be enforced")
	}
}

type fakeOverrideStore struct {
	Store
	runs []store.Run
}

func (f *fakeOverrideStore) ListReserveOverrideRuns(context.Context, uuid.UUID) ([]store.Run, error) {
	return f.runs, nil
}

// TestReserveEndedOverridesSet: the claim's @reserve_override_ended list holds exactly the queued
// runs whose exemption is still inside its clock window but which a reading has ended; a clock-expired
// run is left to ClaimRun's own time test, and an OFF snapshot yields an empty (non-nil) list.
func TestReserveEndedOverridesSet(t *testing.T) {
	now := reserveT0
	anchor := ovAnchor
	moved := anchor.Add(7 * 24 * time.Hour)
	afterGrant := ovGrant.Add(time.Hour)
	endedEarly := ovRun("claude", anchor, &anchor)
	endedEarly.ID = uuid.New()
	active := ovRun("claude", anchor, &anchor)
	active.ID = uuid.New()
	expired := ovRun("claude", now.Add(-time.Minute), &anchor)
	expired.ID = uuid.New()
	svc := &Service{q: &fakeOverrideStore{runs: []store.Run{endedEarly, active, expired}}}

	snap := ovSnap(freshV(usagereserve.Held, 55, moved), afterGrant)
	got, err := svc.reserveEndedOverrides(context.Background(), snap, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != endedEarly.ID || got[1] != active.ID {
		// both early-ended: the reading moved the boundary for every anchored run on this credential.
		t.Fatalf("ended = %v, want the two in-window runs the reading ended", got)
	}
	snap = ovSnap(freshV(usagereserve.Held, 60, anchor), afterGrant)
	if got, _ = svc.reserveEndedOverrides(context.Background(), snap, now); len(got) != 0 {
		t.Fatalf("ended = %v, want none while the boundary is unmoved", got)
	}
	off := &reserveSnap{userID: uuid.New()}
	if got, _ = svc.reserveEndedOverrides(context.Background(), off, now); got == nil || len(got) != 0 {
		t.Fatalf("OFF snapshot ended = %#v, want an empty non-nil list", got)
	}
}

// TestServedPauseMode: a reserve request reaches a worker as a milestone request; no other mode is
// rewritten.
func TestServedPauseMode(t *testing.T) {
	for in, want := range map[string]string{"reserve": "milestone", "milestone": "milestone", "now": "now", "wall": "wall", "": ""} {
		if got := servedPauseMode(in); got != want {
			t.Errorf("servedPauseMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReserveSpentVerdictAndRequestWanted: the verdict is read from the credential the run RECORDED
// (Codex alias for a Codex run), a missing credential reads as none, and a pending request is kept
// through an unknown reading and dropped on a fresh OK one, the cleared threshold or the toggle off.
func TestReserveSpentVerdictAndRequestWanted(t *testing.T) {
	snap := handSnap()
	snap.claude[tokPinHi] = usagereserve.Verdict{State: usagereserve.Held, Pct: 80, Threshold: 70, Fresh: true}
	snap.claude[tokPinOK] = usagereserve.Verdict{State: usagereserve.OK, Pct: 5, Threshold: 70, Fresh: true}
	snap.codex[aliasHi] = usagereserve.Verdict{State: usagereserve.Held, Pct: 95, Threshold: 70, Fresh: true}

	if v, ok := snap.spentVerdict(store.Run{AnthropicSecretID: pgconv.UUID(tokPinHi)}); !ok || v.State != usagereserve.Held {
		t.Errorf("claude spent verdict = (%v, %v)", v, ok)
	}
	if v, ok := snap.spentVerdict(store.Run{Harness: harnessCodex, CodexSecretID: pgconv.UUID(aliasHi), AnthropicSecretID: pgconv.UUID(tokPinOK)}); !ok || v.State != usagereserve.Held {
		t.Errorf("codex run read the wrong credential: (%v, %v)", v, ok)
	}
	if _, ok := snap.spentVerdict(store.Run{}); ok {
		t.Error("a run with no recorded credential has a verdict")
	}
	if _, ok := snap.spentVerdict(store.Run{AnthropicSecretID: pgconv.UUID(uuid.New())}); ok {
		t.Error("an unknown credential has a verdict")
	}

	run := store.Run{AnthropicSecretID: pgconv.UUID(tokPinHi)}
	if !reserveRequestStillWanted(run, snap) {
		t.Error("a held reading must keep the request")
	}
	snap.claude[tokPinHi] = unknownV()
	if !reserveRequestStillWanted(run, snap) {
		t.Error("an unknown reading must keep the request (never withdraw on freshness)")
	}
	if reserveRequestStillWanted(store.Run{AnthropicSecretID: pgconv.UUID(tokPinOK)}, snap) {
		t.Error("a fresh OK reading must withdraw the request")
	}
	snap.pauseRunning = false
	if reserveRequestStillWanted(run, snap) {
		t.Error("the toggle off must withdraw the request")
	}
	snap.pauseRunning = true
	snap.threshold = nil
	if reserveRequestStillWanted(run, snap) {
		t.Error("a cleared threshold must withdraw the request")
	}
}

func TestWorkerVisibleHoldReason(t *testing.T) {
	s := func(v string) *string { return &v }
	if got := WorkerVisibleHoldReason(s("weekly_reserve")); got != nil {
		t.Errorf("weekly_reserve = %q, want nil", *got)
	}
	if got := WorkerVisibleHoldReason(nil); got != nil {
		t.Errorf("nil = %q, want nil", *got)
	}
	for _, hold := range []string{"budget_exhausted", "completion_blocked", "provider_unavailable", "credential_disabled", "owner"} {
		if got := WorkerVisibleHoldReason(s(hold)); got == nil || *got != hold {
			t.Errorf("%s = %v, want unchanged", hold, got)
		}
	}
}

// TestReserveReleaseHasCapacity: with no lane that could claim the run, the lane loop proves nothing,
// so an enforced reserve releases only on a fresh OK reading of the credential the run spent.
func TestReserveReleaseHasCapacity(t *testing.T) {
	now := reserveT0
	anchor := ovAnchor
	run := store.Run{
		Kind: runkind.Issue, Harness: "claude", Status: "paused",
		AnthropicSecretID: pgconv.UUID(tokDefault),
	}
	svc := &Service{}
	// ephemeral=false and no workers: no lane.
	if got := svc.reserveReleaseHasCapacity(run, ovSnap(freshV(usagereserve.Held, 60, anchor), now), nil, false, false, now); got {
		t.Error("no lane and a held reading must not release")
	}
	if got := svc.reserveReleaseHasCapacity(run, ovSnap(usagereserve.Verdict{State: usagereserve.Unknown}, now), nil, false, false, now); got {
		t.Error("no lane and an unknown reading must not release")
	}
	if got := svc.reserveReleaseHasCapacity(run, ovSnap(freshV(usagereserve.OK, 10, anchor), now), nil, false, false, now); !got {
		t.Error("no lane and a fresh OK reading must release")
	}
	off := &reserveSnap{userID: uuid.New()}
	if got := svc.reserveReleaseHasCapacity(run, off, nil, false, false, now); !got {
		t.Error("a reserve that is not enforced must release")
	}
	// With a lane (ephemeral on) the helper defers to the lanes.
	if got := svc.reserveReleaseHasCapacity(run, ovSnap(freshV(usagereserve.Held, 60, anchor), now), nil, false, true, now); !got {
		t.Error("with a lane the helper defers to the lane decision")
	}
	// Greptile #3568: the credential the run spent was deleted while it was paused (absent from the
	// snapshot). Even with a lane, an enforced reserve keeps the pause for the owner rather than
	// releasing it into a claim that can only fail.
	gone := run
	gone.AnthropicSecretID = pgconv.UUID(uuid.New())
	if got := svc.reserveReleaseHasCapacity(gone, ovSnap(freshV(usagereserve.OK, 10, anchor), now), nil, false, true, now); got {
		t.Error("a deleted spent credential must not release, even with a lane")
	}
	gone.AnthropicSecretID = pgtype.UUID{}
	if got := svc.reserveReleaseHasCapacity(gone, ovSnap(freshV(usagereserve.OK, 10, anchor), now), nil, false, true, now); got {
		t.Error("a cleared spent credential must not release, even with a lane")
	}
	if got := svc.reserveReleaseHasCapacity(gone, off, nil, false, true, now); !got {
		t.Error("a reserve that is not enforced still releases a run whose credential is gone")
	}
}

// TestOverrideResetSurvivesThresholdChange: the reset detector's prior is the utilization the grant
// read (reserve_exempt_prior_pct), not the CURRENT threshold. Granted above 70%, the owner then
// lowers the threshold to 1%; a fresh reading at 0% under the unchanged boundary is still an observed
// reset and must end the override, for both harnesses.
func TestOverrideResetSurvivesThresholdChange(t *testing.T) {
	for _, harness := range []string{"claude", harnessCodex} {
		t.Run(harness, func(t *testing.T) {
			run := ovRun(harness, ovAnchor, &ovAnchor)
			run.ReserveExemptPriorPct = pgtype.Int2{Int16: 72, Valid: true}
			snap := ovSnap(freshV(usagereserve.OK, 0, ovAnchor), ovGrant.Add(time.Hour))
			snap.threshold = i16p(1)
			if overrideActive(run, snap, reserveT0) {
				t.Fatal("an observed reset must end the override after the threshold is lowered from 70 to 1")
			}
			// The same reading with the threshold raised still ends it, and an unchanged 72% reading does not.
			snap.threshold = i16p(90)
			if overrideActive(run, snap, reserveT0) {
				t.Fatal("an observed reset must end the override after the threshold is raised")
			}
			still := ovSnap(freshV(usagereserve.OK, 72, ovAnchor), ovGrant.Add(time.Hour))
			still.threshold = i16p(1)
			if !overrideActive(run, still, reserveT0) {
				t.Fatal("an unchanged reading under the same boundary is not a reset")
			}
		})
	}
}

// TestOverrideAnchorWithoutPriorUsesBoundaryOnly: a row carrying an anchor but no recorded prior
// cannot prove a drop, so an unmoved boundary keeps the exemption while a moved one still ends it.
func TestOverrideAnchorWithoutPriorUsesBoundaryOnly(t *testing.T) {
	moved := ovAnchor.Add(7 * 24 * time.Hour)
	for _, harness := range []string{"claude", harnessCodex} {
		run := ovRun(harness, ovAnchor, &ovAnchor)
		run.ReserveExemptPriorPct = pgtype.Int2{}
		if !overrideActive(run, ovSnap(freshV(usagereserve.OK, 0, ovAnchor), ovGrant.Add(time.Hour)), reserveT0) {
			t.Errorf("%s: no recorded prior, unmoved boundary: want the exemption kept", harness)
		}
		if overrideActive(run, ovSnap(freshV(usagereserve.OK, 0, moved), ovGrant.Add(time.Hour)), reserveT0) {
			t.Errorf("%s: no recorded prior, moved boundary: want the exemption ended", harness)
		}
	}
}

// TestCodexOverrideIgnoresWindowSwitch (Greptile #3568): granted on weekly window A at 90%, the
// override survives window B overtaking A (the verdict's reset becomes B's), and ends when A resets.
func TestCodexOverrideIgnoresWindowSwitch(t *testing.T) {
	b := ovAnchor.Add(72 * time.Hour)
	run := ovRun(harnessCodex, ovAnchor, &ovAnchor)
	run.ReserveExemptPriorPct = pgtype.Int2{Int16: 90, Valid: true}
	switched := freshV(usagereserve.Held, 91, b)
	switched.Weekly = []usagereserve.WeeklyWindow{{ResetsAt: &ovAnchor, Pct: 90}, {ResetsAt: &b, Pct: 91}}
	if !overrideActive(run, ovSnap(switched, ovGrant.Add(time.Hour)), reserveT0) {
		t.Fatal("window B overtaking A ended the override although A did not reset")
	}
	rolled := ovAnchor.Add(7 * 24 * time.Hour)
	reset := freshV(usagereserve.Held, 91, b)
	reset.Weekly = []usagereserve.WeeklyWindow{{ResetsAt: &rolled, Pct: 2}, {ResetsAt: &b, Pct: 91}}
	if overrideActive(run, ovSnap(reset, ovGrant.Add(time.Hour)), reserveT0) {
		t.Fatal("window A rolling forward must end the override")
	}
}
