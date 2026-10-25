package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3454 U2: a queued run held by the instance intake pause reports reasonMaintenance, never
// nudges, and leaving the hold starts a new health episode.

func TestHealthIntakePauseReasonNoNudge(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(15 * time.Minute)
	r.IntakePauseHeld = true
	r.Kind = "task" // undispatched: the maintenance reason must outrank the handoff rung
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1, freeSlotWorkers: 1}
	svc := healthSvc(fs, defaultHealthSettings())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker || w.HealthReason.String != reasonMaintenance {
		t.Fatalf("health/reason = %q/%q, want waiting_worker/%q", w.Health, w.HealthReason.String, reasonMaintenance)
	}
	if w.HealthNotifiedAt.Valid {
		t.Fatalf("health_notified_at = %v, want NULL (maintenance never nudges)", w.HealthNotifiedAt)
	}
	if len(b.healthNudges) != 1 || b.healthNudges[0] {
		t.Fatalf("broadcast nudge = %v, want [false]", b.healthNudges)
	}
}

func TestHealthIntakePauseNotHeldKeepsWorkerReason(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(15 * time.Minute)
	r.IntakePauseHeld = false // an admitted run (or no pause) projects false
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 0}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)
	if got := lastWrite(t, fs, r.ID).HealthReason.String; got != reasonNoWorker {
		t.Fatalf("reason = %q, want %q", got, reasonNoWorker)
	}
}

// After the lift health_since is restamped but the run does not nudge on that tick (no Slack DM
// burst for the whole backlog); a run still stuck after the lift nudges only on a later
// ok->flagged episode (accepted; PRD #3454 D12).
func TestHealthLeftMaintenanceRestampsWithoutNudge(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(40 * time.Minute)
	r.Health = healthWaitingWorker
	r.HealthReason = pgconv.TextOrNull(reasonMaintenance)
	r.HealthSince = ago(30 * time.Minute)
	r.IntakePauseHeld = false
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 0}
	svc := healthSvc(fs, defaultHealthSettings())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String != reasonNoWorker {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonNoWorker)
	}
	if !w.HealthSince.Valid || !w.HealthSince.Time.Equal(t0) {
		t.Fatalf("health_since = %v, want restamped to now %v (the pause must not count as stuck time)", w.HealthSince, t0)
	}
	if len(b.healthNudges) != 1 || b.healthNudges[0] || w.HealthNotifiedAt.Valid {
		t.Fatalf("nudge = %v notifiedAt = %v, want NO nudge on the lift tick", b.healthNudges, w.HealthNotifiedAt)
	}
}
