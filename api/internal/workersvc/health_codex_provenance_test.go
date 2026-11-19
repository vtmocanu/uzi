package workersvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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
