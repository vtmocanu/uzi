package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestHealthCustodyLimitNudgeSuppressed pins PRD #1349 M6 (D10): a queued run whose owner is at
// the custody-hold admission limit still gets its per-run health STATE written and broadcast (the
// pill is unchanged — waiting_worker / reasonCustodyLimit), but its per-run Slack NUDGE is
// SUPPRESSED, because the owner-level custody-episode reconciler coalesces that crossing into one
// owner DM. Without the suppression this run's first flag would nudge (r.Health==ok, no prior
// health_notified_at), so the false nudge below is the discriminating assertion.
func TestHealthCustodyLimitNudgeSuppressed(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(15 * time.Minute) // > 10m queued → past the threshold
	fs := &healthFakeStore{
		active:        []store.ListActiveRunsForHealthRow{r},
		custodyHolds:  int64(custodyHoldLimit), // at the limit → reasonCustodyLimit
		onlineWorkers: 0,
	}
	svc := healthSvc(fs, defaultHealthSettings()) // cooldown 30m, but no prior nudge → would nudge
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (the STATE is still written)", n)
	}
	// STATE/pill unchanged: the waiting_worker flag + the custody reason are persisted.
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker {
		t.Fatalf("health = %q, want waiting_worker (the pill must be unchanged)", w.Health)
	}
	if w.HealthReason.String != reasonCustodyLimit {
		t.Fatalf("reason = %q, want %q (the pill must be unchanged)", w.HealthReason.String, reasonCustodyLimit)
	}
	// health_notified_at must NOT be stamped — no per-run nudge means the cooldown is not burned,
	// so a later non-custody flag on the same run can still nudge on its own merits.
	if w.HealthNotifiedAt.Valid {
		t.Fatalf("health_notified_at = %v, want NULL (a suppressed nudge must not stamp the cooldown)", w.HealthNotifiedAt)
	}
	// The broadcast fires (the web pill updates) but carries nudge=false, so slacksvc's
	// handleHealth posts no threaded DM for this run.
	if len(b.healths) != 1 || b.healths[0] != healthWaitingWorker {
		t.Fatalf("broadcast healths = %v, want [waiting_worker]", b.healths)
	}
	if len(b.healthNudges) != 1 || b.healthNudges[0] {
		t.Fatalf("broadcast nudge = %v, want [false] (the per-run custody nudge is suppressed)", b.healthNudges)
	}
}

// TestHealthNonCustodyQueuedReasonStillNudges is the discriminating control: a queued run blocked
// for a NON-custody reason (no worker online) DOES nudge on its first flag. The M6 suppression is
// keyed strictly to reasonCustodyLimit, not to the shared waiting_worker enum, so every other
// queued reason keeps its per-run nudge and stamps the cooldown.
func TestHealthNonCustodyQueuedReasonStillNudges(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(15 * time.Minute)
	fs := &healthFakeStore{
		active:        []store.ListActiveRunsForHealthRow{r},
		custodyHolds:  int64(custodyHoldLimit) - 1, // below the limit → custody rung falls through
		onlineWorkers: 0,                           // → reasonNoWorker
	}
	svc := healthSvc(fs, defaultHealthSettings())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker {
		t.Fatalf("health = %q, want waiting_worker", w.Health)
	}
	if w.HealthReason.String != reasonNoWorker {
		t.Fatalf("reason = %q, want %q (the control reason)", w.HealthReason.String, reasonNoWorker)
	}
	if len(b.healthNudges) != 1 || !b.healthNudges[0] {
		t.Fatalf("broadcast nudge = %v, want [true] (a non-custody queued reason still nudges)", b.healthNudges)
	}
	if !w.HealthNotifiedAt.Valid {
		t.Fatal("health_notified_at not stamped for a real (non-custody) per-run nudge")
	}
}
