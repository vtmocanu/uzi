package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

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
