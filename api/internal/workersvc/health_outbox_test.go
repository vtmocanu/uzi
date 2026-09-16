package workersvc

// PRD #1391 M5 — the outbox arm of runningTarget's stalled branch. A run whose worker
// is holding its updates during an api outage looks silent to the health detector,
// which would read that silence as `stalled` — the exact wrong signal the outbox
// exists to prevent. These pin that a reported non-zero pending depth turns the reason
// into reasonOutboxQueued, and that it reverts to reasonStalled once the depth clears.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHealthStalledBecomesOutboxQueuedWhenDepthReported(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)      // well under the near-timeout threshold
	r.LastActivityAt = ago(10 * time.Minute) // silent 10m > 5m stall
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	// A worker reported a non-zero pending outbox depth for THIS run.
	svc.outbox.record(uuid.New(), []OutboxEntry{
		{RunID: r.ID, PendingMessages: 4, Since: t0.Add(-time.Minute)},
	}, t0)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthStalled {
		t.Fatalf("health = %q, want stalled (same enum, truthful reason)", w.Health)
	}
	if !w.HealthReason.Valid || w.HealthReason.String != reasonOutboxQueued {
		t.Fatalf("reason = %q, want %q: a stalled run whose worker is holding its updates must read the queued reason, not the misleading stall",
			w.HealthReason.String, reasonOutboxQueued)
	}
}

func TestHealthStalledStaysStalledWithoutOutboxDepth(t *testing.T) {
	// Control: the SAME stalled fixture with no reported depth keeps reasonStalled, so
	// the test above proves the outbox arm, not the stall arm.
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)
	r.LastActivityAt = ago(10 * time.Minute)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthStalled || w.HealthReason.String != reasonStalled {
		t.Fatalf("health=%q reason=%q, want stalled/%q", w.Health, w.HealthReason.String, reasonStalled)
	}
}

func TestHealthStalledIgnoresZeroPendingOutboxDepth(t *testing.T) {
	// A tracked entry with ZERO pending messages (e.g. only stale_retired counted) is
	// not "queued": the arm keys on PendingMessages > 0, so a zero-pending run stays on
	// the plain stalled reason.
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)
	r.LastActivityAt = ago(10 * time.Minute)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.outbox.record(uuid.New(), []OutboxEntry{
		{RunID: r.ID, PendingMessages: 0, StaleRetired: 3, Since: t0.Add(-time.Minute)},
	}, t0)

	svc.detectRunHealth(context.Background(), t0)
	if got := lastWrite(t, fs, r.ID).HealthReason.String; got != reasonStalled {
		t.Fatalf("reason = %q, want %q: zero pending is not queued", got, reasonStalled)
	}
}

func TestHealthOutboxQueuedRevertsToStalledWhenDrained(t *testing.T) {
	// Depth > 0 → queued; the backlog drains (an empty report clears the worker's set)
	// → normal stalled detection resumes on the next tick.
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)
	r.LastActivityAt = ago(10 * time.Minute)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())
	w := uuid.New()
	svc.outbox.record(w, []OutboxEntry{{RunID: r.ID, PendingMessages: 4, Since: t0.Add(-time.Minute)}}, t0)

	svc.detectRunHealth(context.Background(), t0)
	if got := lastWrite(t, fs, r.ID).HealthReason.String; got != reasonOutboxQueued {
		t.Fatalf("precondition reason = %q, want %q", got, reasonOutboxQueued)
	}

	// Drain: an empty report clears the worker's set. Stage the stored row as the
	// queued reason so the clear pass has a change to write.
	svc.outbox.record(w, nil, t0)
	fs.writes = nil
	r.Health = healthStalled
	r.HealthReason = pgconv.TextOrNull(reasonOutboxQueued)
	fs.active = []store.ListActiveRunsForHealthRow{r}

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("clear pass changed = %d, want 1 (queued → stalled)", n)
	}
	if got := lastWrite(t, fs, r.ID).HealthReason.String; got != reasonStalled {
		t.Fatalf("after drain reason = %q, want %q: with the depth cleared, normal stalled detection resumes", got, reasonStalled)
	}
}
