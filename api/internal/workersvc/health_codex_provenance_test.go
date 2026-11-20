package workersvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4476 M2: the health resolver says why a Codex run held by claim provenance stays queued.

const (
	wantHeldA          = "held for worker A: wait for it or cancel the run; only the worker that last ran this Codex run may resume it"
	wantHeldUnavailble = "held: predecessor provenance unavailable; cancel the run; no worker may resume this Codex run"
	wantRestartA       = "held for worker A: restart it or cancel the run; its previous process was released; only the worker that last ran this Codex run may resume it"
)

func heldCodexRun(name string) store.ListActiveRunsForHealthRow {
	r := runRow("queued")
	r.StatusSince = ago(time.Minute)
	r.UpdatedAt = ago(time.Minute)
	r.CodexProvenanceHeld = true
	r.CodexProvenanceWorkerName = name
	return r
}

func TestCodexProvenanceHoldReasonTexts(t *testing.T) {
	if got := codexProvenanceHoldReason("A", false); got != wantHeldA {
		t.Fatalf("valid reason = %q, want %q", got, wantHeldA)
	}
	if got := codexProvenanceHoldReason("", false); got != wantHeldUnavailble {
		t.Fatalf("unavailable reason = %q, want %q", got, wantHeldUnavailble)
	}
	if got := codexProvenanceHoldReason("A", true); got != wantRestartA {
		t.Fatalf("restart reason = %q, want %q", got, wantRestartA)
	}
	if got := codexProvenanceHoldReason("", true); got != wantHeldUnavailble {
		t.Fatalf("restart without provenance = %q, want %q", got, wantHeldUnavailble)
	}
	for _, s := range []string{wantHeldA, wantHeldUnavailble, wantRestartA} {
		if !isCodexProvenanceHoldReason(s) {
			t.Fatalf("%q not recognised", s)
		}
	}
	for _, s := range []string{"", "held for worker A", reasonWaitingWorker, staleRequeuePinReason(t0, "A")} {
		if isCodexProvenanceHoldReason(s) {
			t.Fatalf("%q wrongly recognised", s)
		}
	}
}

// The held reason outranks an overlapping stale-requeue pin and an owner hold, and names the
// recorded worker rather than the worker the run last sat on.
func TestCodexProvenanceHeldOutranksPinsAndNamesRecordedWorker(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*store.ListActiveRunsForHealthRow)
	}{
		{"stale requeue pin", func(r *store.ListActiveRunsForHealthRow) {
			r.StaleRequeuePinnable = true
			r.StaleRequeueWorkerName = "C"
		}},
		{"owner hold", func(r *store.ListActiveRunsForHealthRow) {
			r.OwnerHoldPinnable = true
			r.StaleRequeueWorkerName = "C"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := heldCodexRun("A")
			tc.mutate(&r)
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc, _ := pinSvc(fs, defaultHealthSettings())
			// Precondition: the lower rung would hold the run on its own.
			_, pinned := svc.staleRequeuePin(t0, r)
			_, held := svc.ownerHold(t0, r)
			if !pinned && !held {
				t.Fatal("test row does not trigger the overlapping hold")
			}
			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			if w.Health != healthWaitingWorker || w.HealthReason.String != wantHeldA {
				t.Fatalf("write = (%q, %q), want (waiting_worker, %q)", w.Health, w.HealthReason.String, wantHeldA)
			}
			if strings.Contains(w.HealthReason.String, "worker C") || strings.Contains(w.HealthReason.String, "another worker") {
				t.Fatalf("reason leaks the pinned worker or the pin promise: %q", w.HealthReason.String)
			}
		})
	}
}

// Legacy NULL provenance (empty name) gets the unavailable text, never a waiting/pin promise, and
// the reason is stable across ticks.
func TestCodexProvenanceUnavailableLegacyRow(t *testing.T) {
	r := heldCodexRun("")
	r.StaleRequeuePinnable = true
	r.StaleRequeueWorkerName = "C"
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc, b := pinSvc(fs, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String != wantHeldUnavailble {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, wantHeldUnavailble)
	}
	if strings.Contains(w.HealthReason.String, "waiting until") || strings.Contains(w.HealthReason.String, "another worker may") {
		t.Fatalf("unavailable reason promises another worker: %q", w.HealthReason.String)
	}
	// Even inside the pin window, the unavailable reason always nudges (a new episode from ok).
	if len(b.healthNudges) != 1 || !b.healthNudges[0] {
		t.Fatalf("unavailable reason was not nudged: %v", b.healthNudges)
	}

	r.Health, r.HealthReason, r.HealthSince, r.HealthNotifiedAt = healthWaitingWorker, w.HealthReason, w.HealthSince, w.HealthNotifiedAt
	fs2 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc2, _ := pinSvc(fs2, defaultHealthSettings())
	if n := svc2.detectRunHealth(context.Background(), t0.Add(30*time.Second)); n != 0 {
		t.Fatalf("second tick rewrote the reason (changed=%d)", n)
	}
}

// A young run (1m, far below the queued threshold) carries the held reason from the first tick.
func TestCodexProvenanceHeldYoungRunFlagsImmediately(t *testing.T) {
	r := heldCodexRun("A")
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc, _ := pinSvc(fs, defaultHealthSettings())
	target, reason := svc.youngQueuedTarget(context.Background(), t0, r, svc.healthThresholds(context.Background()))
	if target != healthWaitingWorker || reason != wantHeldA {
		t.Fatalf("young target = (%q, %q), want (waiting_worker, %q)", target, reason, wantHeldA)
	}
	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
}

func TestCodexProvenanceHoldNudgeSuppression(t *testing.T) {
	cases := []struct {
		name      string
		name2     string
		mutate    func(*store.ListActiveRunsForHealthRow)
		wantNudge bool
	}{
		{"valid inside stale pin window", "A", func(r *store.ListActiveRunsForHealthRow) { r.StaleRequeuePinnable = true }, false},
		{"valid inside owner hold window", "A", func(r *store.ListActiveRunsForHealthRow) { r.OwnerHoldPinnable = true }, false},
		{"valid outside any window", "A", func(*store.ListActiveRunsForHealthRow) {}, true},
		{"valid after pin grace expired", "A", func(r *store.ListActiveRunsForHealthRow) {
			r.StaleRequeuePinnable = true
			r.StatusSince = ago(pinTestGrace + time.Minute)
		}, true},
		{"unavailable inside pin window", "", func(r *store.ListActiveRunsForHealthRow) { r.StaleRequeuePinnable = true }, true},
		{"unavailable inside owner hold", "", func(r *store.ListActiveRunsForHealthRow) { r.OwnerHoldPinnable = true }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := heldCodexRun(tc.name2)
			r.HealthNotifiedAt = pgtype.Timestamptz{}
			tc.mutate(&r)
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc, b := pinSvc(fs, defaultHealthSettings())
			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			if !isCodexProvenanceHoldReason(w.HealthReason.String) {
				t.Fatalf("reason = %q, want a provenance hold", w.HealthReason.String)
			}
			got := len(b.healthNudges) == 1 && b.healthNudges[0]
			if got != tc.wantNudge || w.HealthNotifiedAt.Valid != tc.wantNudge {
				t.Fatalf("nudge = %v (notifiedAt=%v), want %v", b.healthNudges, w.HealthNotifiedAt, tc.wantNudge)
			}
		})
	}
}

// A named held row that left another quiet state (maintenance or an expired Codex admission wait)
// with no pinnable flag set is silent until updated_at + the affinity ceiling, the latest end of
// any window that could have covered it; it then nudges exactly once. Never at +30s.
func TestCodexProvenanceHeldQuietTransitionsNudgeAtCeilingEnd(t *testing.T) {
	expired := apitypes.CodexAdmissionReason("rate_limit", t0.Add(-time.Hour))
	cases := []struct {
		name        string
		prior       string
		stalePinned bool
	}{
		{"leaving maintenance", reasonMaintenance, false},
		{"leaving an expired rate limit admission", expired, false},
		// The stale flag stays true after the 20m grace (already over at 30m): it must not end the window.
		{"leaving maintenance, stale flag past grace", reasonMaintenance, true},
		{"leaving an expired admission, stale flag past grace", expired, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := heldCodexRun("A")
			r.StaleRequeuePinnable = tc.stalePinned
			r.StatusSince = ago(30 * time.Minute)
			r.UpdatedAt = ago(30 * time.Minute)
			r.Health = healthWaitingWorker
			r.HealthReason = pgtype.Text{String: tc.prior, Valid: true}
			r.HealthSince = ago(25 * time.Minute)
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc, b := pinSvc(fs, defaultHealthSettings())
			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			if w.HealthReason.String != wantHeldA || w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
				t.Fatalf("tick 1: reason=%q nudges=%v notified=%v, want the held reason without a nudge", w.HealthReason.String, b.healthNudges, w.HealthNotifiedAt)
			}

			r.HealthReason, r.HealthSince, r.HealthNotifiedAt = w.HealthReason, w.HealthSince, w.HealthNotifiedAt
			fs2 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc2, b2 := pinSvc(fs2, defaultHealthSettings())
			svc2.detectRunHealth(context.Background(), t0.Add(30*time.Second))
			if len(fs2.writes) != 0 || len(b2.healthNudges) != 0 {
				t.Fatalf("tick 2: writes=%d nudges=%v, want neither", len(fs2.writes), b2.healthNudges)
			}

			// At updated_at + ceiling (2h in pinSvc) the one owed nudge fires.
			end := r.UpdatedAt.Time.Add(svc2.p.WorkerAffinityCeiling)
			fs3 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc3, b3 := pinSvc(fs3, defaultHealthSettings())
			svc3.detectRunHealth(context.Background(), end)
			w3 := lastWrite(t, fs3, r.ID)
			if len(b3.healthNudges) != 1 || !b3.healthNudges[0] || !w3.HealthNotifiedAt.Valid {
				t.Fatalf("at the ceiling end: nudges=%v notified=%v, want exactly one nudge", b3.healthNudges, w3.HealthNotifiedAt)
			}

			r.HealthNotifiedAt = w3.HealthNotifiedAt
			fs4 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc4, _ := pinSvc(fs4, defaultHealthSettings())
			if n := svc4.detectRunHealth(context.Background(), end.Add(time.Minute)); n != 0 {
				t.Fatalf("after the nudge: changed=%d, want 0", n)
			}
		})
	}
}

// With the ceiling already past before the episode began, no window covers it: such a row never
// gets a late nudge, at tick 2 or after.
func TestCodexProvenanceHeldQuietTransitionsNotCoveredNeverNudge(t *testing.T) {
	for _, prior := range []string{reasonMaintenance, apitypes.CodexAdmissionReason("rate_limit", t0.Add(-time.Hour))} {
		r := heldCodexRun("A")
		r.UpdatedAt = ago(3 * time.Hour)
		r.Health = healthWaitingWorker
		r.HealthReason = pgtype.Text{String: prior, Valid: true}
		r.HealthSince = ago(time.Hour)
		fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
		svc, _ := pinSvc(fs, defaultHealthSettings())
		svc.detectRunHealth(context.Background(), t0)
		w := lastWrite(t, fs, r.ID)
		if w.HealthNotifiedAt.Valid {
			t.Fatalf("tick 1 nudged for prior %q", prior)
		}
		r.HealthReason, r.HealthSince = w.HealthReason, w.HealthSince
		fs2 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
		svc2, b2 := pinSvc(fs2, defaultHealthSettings())
		for _, d := range []time.Duration{30 * time.Second, 4 * time.Hour} {
			svc2.detectRunHealth(context.Background(), t0.Add(d))
		}
		if len(fs2.writes) != 0 || len(b2.healthNudges) != 0 {
			t.Fatalf("prior %q: later ticks wrote=%d nudges=%v, want neither", prior, len(fs2.writes), b2.healthNudges)
		}
	}
}

// Entering the unavailable-provenance hold from a non-ok state nudges (the cooldown permitting).
func TestCodexProvenanceUnavailableNudgesOnEntryFromNonOK(t *testing.T) {
	r := heldCodexRun("")
	r.Health = healthStalled
	r.HealthReason = pgtype.Text{String: reasonOutcomeUndelivered, Valid: true}
	r.HealthSince = ago(2 * time.Hour)
	r.HealthNotifiedAt = ago(time.Hour)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc, b := pinSvc(fs, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String != wantHeldUnavailble || !w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || !b.healthNudges[0] {
		t.Fatalf("reason=%q nudges=%v notified=%v, want one nudge on entry", w.HealthReason.String, b.healthNudges, w.HealthNotifiedAt)
	}
}

// The unavailable reason entered from an expired Codex admission wait still nudges on entry.
func TestCodexProvenanceUnavailableNudgesFromExpiredAdmission(t *testing.T) {
	r := heldCodexRun("")
	r.Health = healthWaitingWorker
	r.HealthReason = pgtype.Text{String: apitypes.CodexAdmissionReason("rate_limit", t0.Add(-time.Hour)), Valid: true}
	r.HealthSince = ago(2 * time.Hour)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc, b := pinSvc(fs, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String != wantHeldUnavailble || !w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || !b.healthNudges[0] {
		t.Fatalf("reason=%q nudges=%v notified=%v, want one nudge on entry", w.HealthReason.String, b.healthNudges, w.HealthNotifiedAt)
	}
}

// The pinnable flags can drop mid-window (worker row replaced, drain ended, origin-owner pins
// flipped); a quiet named hold then nudges exactly once at updated_at + the affinity ceiling, not
// before, and not again.
func TestCodexProvenanceQuietHoldNudgesWhenPinFlagsDrop(t *testing.T) {
	cases := []struct {
		name string
		set  func(*store.ListActiveRunsForHealthRow, bool)
	}{
		{"owner hold", func(r *store.ListActiveRunsForHealthRow, v bool) { r.OwnerHoldPinnable = v }},
		{"stale requeue pin", func(r *store.ListActiveRunsForHealthRow, v bool) { r.StaleRequeuePinnable = v }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := heldCodexRun("A")
			tc.set(&r, true)
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc, b := pinSvc(fs, defaultHealthSettings())
			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
				t.Fatalf("tick 1: nudges=%v notified=%v, want quiet", b.healthNudges, w.HealthNotifiedAt)
			}

			r.Health, r.HealthReason, r.HealthSince = healthWaitingWorker, w.HealthReason, w.HealthSince
			tc.set(&r, false)
			// Flags dropped: no clock names the window's end, so the run waits for updated_at + ceiling.
			fs2 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc2, b2 := pinSvc(fs2, defaultHealthSettings())
			if n := svc2.detectRunHealth(context.Background(), t0.Add(5*time.Minute)); n != 0 || len(b2.healthNudges) != 0 {
				t.Fatalf("tick 2 (+5m): changed=%d nudges=%v, want neither", n, b2.healthNudges)
			}
			end := r.UpdatedAt.Time.Add(svc2.p.WorkerAffinityCeiling)
			fs3 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc3, b3 := pinSvc(fs3, defaultHealthSettings())
			if n := svc3.detectRunHealth(context.Background(), end); n != 1 {
				t.Fatalf("at the ceiling end: changed=%d, want 1", n)
			}
			w3 := lastWrite(t, fs3, r.ID)
			if len(b3.healthNudges) != 1 || !b3.healthNudges[0] || !w3.HealthNotifiedAt.Valid {
				t.Fatalf("at the ceiling end: nudges=%v notified=%v, want exactly one nudge", b3.healthNudges, w3.HealthNotifiedAt)
			}

			r.HealthNotifiedAt = w3.HealthNotifiedAt
			fs4 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc4, _ := pinSvc(fs4, defaultHealthSettings())
			if n := svc4.detectRunHealth(context.Background(), end.Add(time.Minute)); n != 0 {
				t.Fatalf("after the nudge: changed=%d, want 0", n)
			}
		})
	}
}

// A hold that started quiet inside the pin window nudges once after the window ends, then stays
// quiet (the episode is stamped); a hold already nudged this episode is not nudged again.
func TestCodexProvenanceQuietHoldNudgesAfterWindow(t *testing.T) {
	r := heldCodexRun("A")
	r.StaleRequeuePinnable = true
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc, b := pinSvc(fs, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String != wantHeldA || len(b.healthNudges) != 1 || b.healthNudges[0] || w.HealthNotifiedAt.Valid {
		t.Fatalf("first tick: reason=%q nudges=%v notified=%v, want the quiet held reason", w.HealthReason.String, b.healthNudges, w.HealthNotifiedAt)
	}
	if w.MarkOwnerHold || len(fs.marks) != 0 {
		t.Fatalf("stale-only episode marked an owner hold: MarkOwnerHold=%v marks=%d", w.MarkOwnerHold, len(fs.marks))
	}

	// Still inside the window: unchanged, no write.
	r.Health, r.HealthReason, r.HealthSince = healthWaitingWorker, w.HealthReason, w.HealthSince
	fs2 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc2, _ := pinSvc(fs2, defaultHealthSettings())
	if n := svc2.detectRunHealth(context.Background(), t0.Add(time.Minute)); n != 0 {
		t.Fatalf("inside the window: changed=%d, want 0", n)
	}

	// The grace has expired: the same reason now nudges once, keeping health_since.
	after := t0.Add(pinTestGrace + 2*time.Minute)
	fs3 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc3, b3 := pinSvc(fs3, defaultHealthSettings())
	if n := svc3.detectRunHealth(context.Background(), after); n != 1 {
		t.Fatalf("after the window: changed=%d, want 1", n)
	}
	w3 := lastWrite(t, fs3, r.ID)
	if len(b3.healthNudges) != 1 || !b3.healthNudges[0] || !w3.HealthNotifiedAt.Valid || w3.HealthReason.String != wantHeldA {
		t.Fatalf("after the window: nudges=%v notified=%v reason=%q, want one nudge of the held reason", b3.healthNudges, w3.HealthNotifiedAt, w3.HealthReason.String)
	}
	if !w3.HealthSince.Time.Equal(r.HealthSince.Time) {
		t.Fatalf("health_since restamped: %v, want the episode's %v", w3.HealthSince, r.HealthSince)
	}

	// Once nudged this episode, later ticks are unchanged again.
	r.HealthNotifiedAt = w3.HealthNotifiedAt
	fs4 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc4, _ := pinSvc(fs4, defaultHealthSettings())
	if n := svc4.detectRunHealth(context.Background(), after.Add(time.Minute)); n != 0 {
		t.Fatalf("after the nudge: changed=%d, want 0", n)
	}
}

// Issue #4507 helpers: every tick uses a fresh store and service (models an API restart), so the only
// state carried between ticks is what the test feeds back into the row.
func ownerHoldTick(t *testing.T, r store.ListActiveRunsForHealthRow, now time.Time) (*healthFakeStore, *fakeBroadcaster, int64) {
	t.Helper()
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc, b := pinSvc(fs, defaultHealthSettings())
	return fs, b, svc.detectRunHealth(context.Background(), now)
}

// overlappingHeldRun is a quiet named Codex hold covered by both the stale pin (grace pinTestGrace,
// ending t0+19m) and the owner hold (ceiling 2h, ending t0+119m).
func overlappingHeldRun() store.ListActiveRunsForHealthRow {
	r := heldCodexRun("A")
	r.StaleRequeuePinnable = true
	r.OwnerHoldPinnable = true
	return r
}

// The owner hold outlives the stale grace and then clears (a drain ends) before the ceiling: the
// nudge must wait for the ceiling end, not fire when the hold clears.
func TestCodexProvenanceOverlappingOwnerHoldNudgesAtCeiling(t *testing.T) {
	r := overlappingHeldRun()
	graceEnd := t0.Add(pinTestGrace - time.Minute)
	ceilingEnd := r.UpdatedAt.Time.Add(2 * time.Hour)
	drainCleared := graceEnd.Add(10 * time.Minute)
	if !drainCleared.Before(ceilingEnd) {
		t.Fatalf("test timeline broken")
	}

	fs, b, _ := ownerHoldTick(t, r, t0)
	w := lastWrite(t, fs, r.ID)
	if !w.MarkOwnerHold || w.HealthReason.String != wantHeldA || w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
		t.Fatalf("entry: mark=%v reason=%q nudges=%v notified=%v, want a marked quiet entry", w.MarkOwnerHold, w.HealthReason.String, b.healthNudges, w.HealthNotifiedAt)
	}
	r.Health, r.HealthReason, r.HealthSince = healthWaitingWorker, w.HealthReason, w.HealthSince
	r.HealthOwnerHoldActive = true // the projection of the committed mark

	// After the grace, the owner hold still holds: quiet.
	fs, b, n := ownerHoldTick(t, r, graceEnd.Add(time.Minute))
	if n != 0 || len(fs.writes) != 0 || len(b.healthNudges) != 0 || len(fs.marks) != 0 {
		t.Fatalf("after grace, hold in force: changed=%d writes=%d nudges=%v marks=%d, want all quiet", n, len(fs.writes), b.healthNudges, len(fs.marks))
	}

	// The drain clears. The stale shortcut alone would nudge here; the marker defers it to the ceiling.
	r.OwnerHoldPinnable = false
	fs, b, n = ownerHoldTick(t, r, drainCleared)
	if n != 0 || len(fs.writes) != 0 || len(b.healthNudges) != 0 {
		t.Fatalf("drain cleared before the ceiling: changed=%d writes=%d nudges=%v, want no write and no nudge", n, len(fs.writes), b.healthNudges)
	}

	fs, b, n = ownerHoldTick(t, r, ceilingEnd)
	if n != 1 {
		t.Fatalf("at the ceiling end: changed=%d, want 1", n)
	}
	w2 := lastWrite(t, fs, r.ID)
	if len(b.healthNudges) != 1 || !b.healthNudges[0] || !w2.HealthNotifiedAt.Valid || w2.HealthReason.String != wantHeldA {
		t.Fatalf("at the ceiling end: nudges=%v notified=%v reason=%q, want exactly one nudge of the held reason", b.healthNudges, w2.HealthNotifiedAt, w2.HealthReason.String)
	}
	if !w2.HealthSince.Time.Equal(r.HealthSince.Time) {
		t.Fatalf("health_since restamped: %v, want %v", w2.HealthSince, r.HealthSince)
	}

	r.HealthNotifiedAt = w2.HealthNotifiedAt
	if _, _, n := ownerHoldTick(t, r, ceilingEnd.Add(time.Minute)); n != 0 {
		t.Fatalf("after the nudge: changed=%d, want 0", n)
	}
}

// An owner hold that begins mid-episode (the episode entered on the stale pin alone) is marked once
// by the unchanged-skip branch: no health write, no broadcast, not counted.
func TestCodexProvenanceOwnerHoldBeginsMidEpisodeIsMarked(t *testing.T) {
	r := heldCodexRun("A")
	r.StaleRequeuePinnable = true
	fs, _, _ := ownerHoldTick(t, r, t0)
	w := lastWrite(t, fs, r.ID)
	if w.MarkOwnerHold || len(fs.marks) != 0 {
		t.Fatalf("stale-only entry marked: MarkOwnerHold=%v marks=%d", w.MarkOwnerHold, len(fs.marks))
	}
	r.Health, r.HealthReason, r.HealthSince = healthWaitingWorker, w.HealthReason, w.HealthSince

	r.OwnerHoldPinnable = true
	fs, b, n := ownerHoldTick(t, r, t0.Add(time.Minute))
	if n != 0 || len(fs.writes) != 0 || len(b.healths) != 0 {
		t.Fatalf("mid-episode: changed=%d writes=%d broadcasts=%v, want none", n, len(fs.writes), b.healths)
	}
	if len(fs.marks) != 1 {
		t.Fatalf("marks = %d, want 1", len(fs.marks))
	}
	m := fs.marks[0]
	if m.ID != r.ID || m.Status != r.Status || m.Health != r.Health || m.HealthReason != r.HealthReason || m.HealthSince != r.HealthSince {
		t.Fatalf("mark = %+v, want the row's id/status/health/reason/since", m)
	}

	r.HealthOwnerHoldActive = true
	fs, _, _ = ownerHoldTick(t, r, t0.Add(2*time.Minute))
	if len(fs.marks) != 0 || len(fs.writes) != 0 {
		t.Fatalf("already marked: marks=%d writes=%d, want none", len(fs.marks), len(fs.writes))
	}
}

// The hold cleared before the grace expired, with the marker committed: the nudge waits for the
// ceiling, not the grace end.
func TestCodexProvenanceMarkedHoldClearedBeforeGraceNudgesAtCeiling(t *testing.T) {
	r := overlappingHeldRun()
	fs, _, _ := ownerHoldTick(t, r, t0)
	w := lastWrite(t, fs, r.ID)
	r.Health, r.HealthReason, r.HealthSince = healthWaitingWorker, w.HealthReason, w.HealthSince
	r.HealthOwnerHoldActive = true
	r.OwnerHoldPinnable = false // hold cleared; the stale pin still inside grace

	if fs, b, n := ownerHoldTick(t, r, t0.Add(5*time.Minute)); n != 0 || len(fs.writes) != 0 || len(b.healthNudges) != 0 {
		t.Fatalf("inside grace: changed=%d nudges=%v, want quiet", n, b.healthNudges)
	}
	if fs, b, n := ownerHoldTick(t, r, t0.Add(pinTestGrace+5*time.Minute)); n != 0 || len(fs.writes) != 0 || len(b.healthNudges) != 0 {
		t.Fatalf("after grace: changed=%d nudges=%v, want quiet until the ceiling", n, b.healthNudges)
	}
	fs, b, n := ownerHoldTick(t, r, r.UpdatedAt.Time.Add(2*time.Hour))
	if w := lastWrite(t, fs, r.ID); n != 1 || len(b.healthNudges) != 1 || !b.healthNudges[0] || !w.HealthNotifiedAt.Valid {
		t.Fatalf("at the ceiling: changed=%d nudges=%v, want one nudge", n, b.healthNudges)
	}
}

// The marker never applies to the unavailable-provenance reason, which nudges on entry.
func TestCodexProvenanceUnavailableNeverMarksOwnerHold(t *testing.T) {
	r := heldCodexRun("")
	r.OwnerHoldPinnable = true
	r.Health = healthStalled
	r.HealthReason = pgtype.Text{String: reasonOutcomeUndelivered, Valid: true}
	r.HealthSince = ago(2 * time.Hour)
	fs, b, _ := ownerHoldTick(t, r, t0)
	w := lastWrite(t, fs, r.ID)
	if w.MarkOwnerHold || len(fs.marks) != 0 || !w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || !b.healthNudges[0] {
		t.Fatalf("mark=%v marks=%d nudges=%v notified=%v, want an unmarked nudge on entry", w.MarkOwnerHold, len(fs.marks), b.healthNudges, w.HealthNotifiedAt)
	}
}

// Cooldown still gates the deferred nudge: a nudge stamped before this episode began but still
// inside the cooldown at the ceiling end suppresses it (the sole blocker here).
func TestCodexProvenanceMarkedHoldCeilingNudgeRespectsCooldown(t *testing.T) {
	r := overlappingHeldRun()
	r.UpdatedAt = ago(100 * time.Minute) // ceiling end t0+20m, inside the 30m cooldown of a t0-1s nudge
	r.HealthNotifiedAt = pgconv.Time(t0.Add(-time.Second))
	fs, _, _ := ownerHoldTick(t, r, t0)
	w := lastWrite(t, fs, r.ID)
	if !w.MarkOwnerHold || w.HealthNotifiedAt.Valid {
		t.Fatalf("entry: mark=%v notified=%v, want a marked quiet entry", w.MarkOwnerHold, w.HealthNotifiedAt)
	}
	r.Health, r.HealthReason, r.HealthSince = healthWaitingWorker, w.HealthReason, w.HealthSince
	r.HealthOwnerHoldActive = true
	r.OwnerHoldPinnable = false
	fs, b, n := ownerHoldTick(t, r, r.UpdatedAt.Time.Add(2*time.Hour))
	if n != 0 || len(fs.writes) != 0 || len(b.healthNudges) != 0 {
		t.Fatalf("at the ceiling within cooldown: changed=%d nudges=%v, want none", n, b.healthNudges)
	}
}

// The pinnable flag alone is not an owner hold: past updated_at + WorkerAffinityCeiling ownerHold is
// false, so no tick may stamp the marker, whether it writes a new episode or skips an unchanged one.
func TestCodexProvenancePinnableFlagPastCeilingNeverMarks(t *testing.T) {
	r := heldCodexRun("A")
	r.OwnerHoldPinnable = true
	r.UpdatedAt = ago(3 * time.Hour) // ceiling (2h) ended an hour ago

	fs, _, _ := ownerHoldTick(t, r, t0)
	w := lastWrite(t, fs, r.ID)
	if w.MarkOwnerHold || len(fs.marks) != 0 {
		t.Fatalf("new episode: MarkOwnerHold=%v marks=%d, want none past the ceiling", w.MarkOwnerHold, len(fs.marks))
	}

	// Unchanged episode that was already nudged, so the tick takes the skip branch.
	r.Health, r.HealthReason, r.HealthSince = healthWaitingWorker, w.HealthReason, w.HealthSince
	r.HealthNotifiedAt = pgconv.Time(t0)
	fs, _, n := ownerHoldTick(t, r, t0.Add(time.Minute))
	if n != 0 || len(fs.writes) != 0 || len(fs.marks) != 0 {
		t.Fatalf("unchanged episode: changed=%d writes=%d marks=%d, want none past the ceiling", n, len(fs.writes), len(fs.marks))
	}
}

// Without the marker (the hold began and cleared between ticks, or its write failed) the stale
// shortcut still applies: the nudge fires at grace expiry.
func TestCodexProvenanceUnmarkedStaleExpiredNudgesAtGraceEnd(t *testing.T) {
	r := heldCodexRun("A")
	r.StaleRequeuePinnable = true
	fs, _, _ := ownerHoldTick(t, r, t0)
	w := lastWrite(t, fs, r.ID)
	r.Health, r.HealthReason, r.HealthSince = healthWaitingWorker, w.HealthReason, w.HealthSince
	r.HealthOwnerHoldActive = false
	fs, b, n := ownerHoldTick(t, r, t0.Add(pinTestGrace+time.Minute))
	if n != 1 || len(b.healthNudges) != 1 || !b.healthNudges[0] || !lastWrite(t, fs, r.ID).HealthNotifiedAt.Valid {
		t.Fatalf("changed=%d nudges=%v, want one nudge at grace expiry", n, b.healthNudges)
	}
}
