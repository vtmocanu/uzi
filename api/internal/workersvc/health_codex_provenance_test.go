package workersvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4476 M2: the health resolver says why a Codex run held by claim provenance stays queued.

const (
	wantHeldA          = "held for worker A: wait for it or cancel the run; only the worker that last ran this Codex run may resume it"
	wantHeldUnavailble = "held: predecessor provenance unavailable; cancel the run; no worker may resume this Codex run"
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
	if got := codexProvenanceHoldReason("A"); got != wantHeldA {
		t.Fatalf("valid reason = %q, want %q", got, wantHeldA)
	}
	if got := codexProvenanceHoldReason(""); got != wantHeldUnavailble {
		t.Fatalf("unavailable reason = %q, want %q", got, wantHeldUnavailble)
	}
	for _, s := range []string{wantHeldA, wantHeldUnavailble} {
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

// A named held row that was quieted by another transition (leaving maintenance or an expired Codex
// admission wait) is never nudged afterwards: no stale-requeue or owner-hold window covered the
// episode, so there is no post-window nudge owed.
func TestCodexProvenanceHeldQuietTransitionsNeverLateNudge(t *testing.T) {
	cases := []struct {
		name  string
		prior string
	}{
		{"leaving maintenance", reasonMaintenance},
		{"leaving an expired rate limit admission", apitypes.CodexAdmissionReason("rate_limit", t0.Add(-time.Hour))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := heldCodexRun("A")
			r.Health = healthWaitingWorker
			r.HealthReason = pgtype.Text{String: tc.prior, Valid: true}
			r.HealthSince = ago(time.Hour)
			// Queued long before: the affinity ceiling (2h in pinSvc) ended before the episode began, so
			// no window covers it. With a recent updated_at the time window would still be open at
			// health_since and the hold would nudge once at that window's end (accepted).
			r.UpdatedAt = ago(3 * time.Hour)
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
		})
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
// flipped); a quiet named hold must then still nudge exactly once, and not again.
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
			fs2 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc2, b2 := pinSvc(fs2, defaultHealthSettings())
			if n := svc2.detectRunHealth(context.Background(), t0.Add(5*time.Minute)); n != 1 {
				t.Fatalf("tick 2: changed=%d, want 1", n)
			}
			w2 := lastWrite(t, fs2, r.ID)
			if len(b2.healthNudges) != 1 || !b2.healthNudges[0] || !w2.HealthNotifiedAt.Valid {
				t.Fatalf("tick 2: nudges=%v notified=%v, want exactly one nudge", b2.healthNudges, w2.HealthNotifiedAt)
			}

			r.HealthNotifiedAt = w2.HealthNotifiedAt
			fs3 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc3, _ := pinSvc(fs3, defaultHealthSettings())
			if n := svc3.detectRunHealth(context.Background(), t0.Add(10*time.Minute)); n != 0 {
				t.Fatalf("tick 3: changed=%d, want 0", n)
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
